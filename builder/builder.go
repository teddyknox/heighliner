package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/mod/modfile"

	"github.com/strangelove-ventures/heighliner/docker"
	"github.com/strangelove-ventures/heighliner/dockerfile"
)

type HeighlinerBuilder struct {
	buildConfig HeighlinerDockerBuildConfig
	queue       []HeighlinerQueuedChainBuilds
	parallel    int16
	local       bool
	race        bool

	buildIndex   int
	buildIndexMu sync.Mutex

	errors     []error
	errorsLock sync.Mutex

	tmpDirsToRemove map[string]bool
	tmpDirMapMu     sync.Mutex
}

func NewHeighlinerBuilder(
	buildConfig HeighlinerDockerBuildConfig,
	parallel int16,
	local bool,
	race bool,
) *HeighlinerBuilder {
	return &HeighlinerBuilder{
		buildConfig: buildConfig,
		parallel:    parallel,
		local:       local,
		race:        race,

		tmpDirsToRemove: make(map[string]bool),
	}
}

func (h *HeighlinerBuilder) AddToQueue(chainBuilds ...HeighlinerQueuedChainBuilds) {
	h.queue = append(h.queue, chainBuilds...)
}

func (h *HeighlinerBuilder) QueueLen() int {
	return len(h.queue)
}

// imageTag determines which docker image tag to use based on inputs.
func imageTag(ref string, tag string, local bool) string {
	if tag != "" {
		return tag
	}

	tag = deriveTagFromRef(ref)

	if local && tag == "" {
		return "local"
	}

	return tag
}

// deriveTagFromRef returns a sanitized docker image tag from a git ref (branch/tag).
func deriveTagFromRef(version string) string {
	return strings.ReplaceAll(version, "/", "-")
}

// dockerfileEmbeddedOrLocal attempts to find Dockerfile within current working directory.
// Returns embedded Dockerfile if local file is not found or cannot be read.
func dockerfileEmbeddedOrLocal(dockerfile string, embedded []byte) []byte {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("Using embedded %s due to working directory not found\n", dockerfile)
		return embedded
	}

	absDockerfile := filepath.Join(cwd, "dockerfile", dockerfile)
	if _, err := os.Stat(absDockerfile); err != nil {
		fmt.Printf("Using embedded %s due to local dockerfile not found\n", dockerfile)
		return embedded
	}

	df, err := os.ReadFile(absDockerfile)
	if err != nil {
		fmt.Printf("Using embedded %s due to failure to read local dockerfile\n", dockerfile)
		return embedded
	}

	fmt.Printf("Using local %s\n", dockerfile)
	return df
}

// dockerfileAndTag returns the appropriate dockerfile as bytes and the docker image tag
// based on the input configuration.
func rawDockerfile(
	dockerfileType DockerfileType,
	useBuildKit bool,
	local bool,
) []byte {
	switch dockerfileType {
	case DockerfileTypeImported:
		return dockerfileEmbeddedOrLocal("imported/Dockerfile", dockerfile.Imported)

	case DockerfileTypeCargo:
		if useBuildKit {
			return dockerfileEmbeddedOrLocal("cargo/Dockerfile", dockerfile.Cargo)
		}
		return dockerfileEmbeddedOrLocal("cargo/native.Dockerfile", dockerfile.CargoNative)

	case DockerfileTypeCosmos:
		if local {
			// local builds always use embedded Dockerfile.
			return dockerfile.CosmosLocal
		}
		if useBuildKit {
			return dockerfileEmbeddedOrLocal("cosmos/Dockerfile", dockerfile.Cosmos)
		}
		return dockerfileEmbeddedOrLocal("cosmos/native.Dockerfile", dockerfile.CosmosNative)
	case DockerfileTypeAvalanche:
		if useBuildKit {
			return dockerfileEmbeddedOrLocal("avalanche/Dockerfile", dockerfile.Avalanche)
		}
		return dockerfileEmbeddedOrLocal("avalanche/native.Dockerfile", dockerfile.AvalancheNative)
	default:
		return dockerfileEmbeddedOrLocal("none/Dockerfile", dockerfile.None)
	}
}

// baseImageForGoVersion will determine the go version in go.mod and return the base image
func baseImageForGoVersion(
	repoHost string,
	organization string,
	repoName string,
	ref string,
	buildDir string,
	local bool,
) (GoVersion, error) {
	var goModBz []byte
	var err error

	goModPath := "go.mod"
	if buildDir != "" {
		goModPath = filepath.Join(buildDir, goModPath)
	}

	if local {
		goModBz, err = os.ReadFile(goModPath)
		if err != nil {
			return GoVersion{}, fmt.Errorf("failed to read %s for local build: %w", goModPath, err)
		}
	} else {
		// single branch depth 1 clone to only fetch most recent state of files
		cloneOpts := &git.CloneOptions{
			URL:          fmt.Sprintf("https://%s/%s/%s", repoHost, organization, repoName),
			SingleBranch: true,
			Depth:        1,
		}
		// Try as tag ref first
		cloneOpts.ReferenceName = plumbing.NewTagReferenceName(ref)

		// Clone into memory
		fs := memfs.New()

		_, err = git.Clone(memory.NewStorage(), fs, cloneOpts)
		if err != nil {
			// In error case, try as branch ref
			cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(ref)

			_, err := git.Clone(memory.NewStorage(), fs, cloneOpts)
			if err != nil {
				return GoVersion{}, fmt.Errorf("failed to clone go.mod file to determine go version: %w", err)
			}
		}

		goModFile, err := fs.Open(goModPath)
		if err != nil {
			return GoVersion{}, fmt.Errorf("failed to open go.mod file: %w", err)
		}

		goModBz, err = io.ReadAll(goModFile)
		if err != nil {
			return GoVersion{}, fmt.Errorf("failed to read go.mod file: %w", err)
		}
	}

	goMod, err := modfile.Parse("go.mod", goModBz, nil)
	if err != nil {
		return GoVersion{}, fmt.Errorf("failed to parse go.mod file: %w", err)
	}

	goVersion := goMod.Go.Version
	baseImageVersion := GetImageAndVersionForGoVersion(goVersion)
	fmt.Printf("Go version from go.mod: %s, will build with version: %s image: %s\n", goVersion, baseImageVersion.Version, baseImageVersion.Image)

	return baseImageVersion, nil
}

// buildChainNodeDockerImage builds the requested chain node docker image
// based on the input configuration.
func (h *HeighlinerBuilder) buildChainNodeDockerImage(
	chainConfig *ChainNodeDockerBuildConfig,
) error {
	buildCfg := h.buildConfig
	dockerfile := chainConfig.Build.Dockerfile

	// DEPRECATION HANDLING
	if chainConfig.Build.Language != "" {
		fmt.Printf("'language' chain config property is deprecated, please use 'dockerfile' instead\n")
		if dockerfile == "" {
			dockerfile = chainConfig.Build.Language
		}
	}

	for _, rep := range deprecationReplacements {
		if dockerfile == rep[0] {
			fmt.Printf("'dockerfile' value of '%s' is deprecated, please use '%s' instead\n", rep[0], rep[1])
			dockerfile = rep[1]
		}
	}
	// END DEPRECATION HANDLING

	df := rawDockerfile(dockerfile, buildCfg.UseBuildKit, h.local)

	tag := imageTag(chainConfig.Ref, chainConfig.Tag, h.local)

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("error getting working directory: %w", err)
	}

	dir, err := os.MkdirTemp(cwd, "heighliner")
	if err != nil {
		return fmt.Errorf("error making temporary directory for dockerfile: %w", err)
	}

	// queue removal on ctrl+c
	h.queueTmpDirRemoval(dir, true)
	defer func() {
		// this build is done, so don't need removal on ctrl+c anymore since we are removing now.
		h.queueTmpDirRemoval(dir, false)
		_ = os.RemoveAll(dir)
	}()

	reldir, err := filepath.Rel(cwd, dir)
	if err != nil {
		return fmt.Errorf("error finding relative path for dockerfile working directory: %w", err)
	}

	dfilepath := filepath.Join(reldir, "Dockerfile")
	if err := os.WriteFile(dfilepath, df, 0644); err != nil {
		return fmt.Errorf("error writing temporary dockerfile: %w", err)
	}

	var imageName string
	if buildCfg.ContainerRegistry == "" {
		imageName = chainConfig.Build.Name
	} else {
		imageName = fmt.Sprintf("%s/%s", buildCfg.ContainerRegistry, chainConfig.Build.Name)
	}

	imageTags := []string{fmt.Sprintf("%s:%s", imageName, tag)}
	if chainConfig.Latest {
		imageTags = append(imageTags, fmt.Sprintf("%s:latest", imageName))
	}

	buildFrom := "ref: " + chainConfig.Ref
	if h.local {
		buildFrom = "current working directory source"
	}

	buildEnv := ""

	buildTagsEnvVar := ""
	for _, envVar := range chainConfig.Build.BuildEnv {
		envVarSplit := strings.Split(envVar, "=")
		if envVarSplit[0] == "BUILD_TAGS" {
			buildTagsEnvVar = envVar
		} else {
			buildEnv += envVar + " "
		}
	}

	binaries := strings.Join(chainConfig.Build.Binaries, ",")

	libraries := strings.Join(chainConfig.Build.Libraries, " ")

	targetLibraries := strings.Join(chainConfig.Build.TargetLibraries, " ")

	directories := strings.Join(chainConfig.Build.Directories, " ")

	repoHost := chainConfig.Build.RepoHost
	if repoHost == "" {
		repoHost = "github.com"
	}

	buildTimestamp := ""
	if buildCfg.NoBuildCache {
		buildTimestamp = strconv.FormatInt(time.Now().Unix(), 10)
	}

	var baseVersion string

	baseVer, err := baseImageForGoVersion(
		repoHost, chainConfig.Build.GithubOrganization, chainConfig.Build.GithubRepo,
		chainConfig.Ref, chainConfig.Build.BuildDir, h.local,
	)

	race := ""

	if dockerfile == DockerfileTypeCosmos || dockerfile == DockerfileTypeAvalanche {
		baseVersion = GoDefaultImage // default, and fallback if go.mod parse fails

		// In error case, fallback to default image
		if err != nil {
			fmt.Println(err)
		} else {
			baseVersion = baseVer.Image
		}

		if h.race {
			race = "true"
			buildEnv += " GOFLAGS=-race"
			for i, imageTag := range imageTags {
				imageTags[i] = imageTag + "-race"
			}
		}
	}

	fmt.Printf("Building image from %s, resulting docker image tags: +%v\n", buildFrom, imageTags)

	buildArgs := map[string]string{
		"VERSION":             chainConfig.Ref,
		"BASE_VERSION":        baseVersion,
		"NAME":                chainConfig.Build.Name,
		"BASE_IMAGE":          chainConfig.Build.BaseImage,
		"REPO_HOST":           repoHost,
		"GITHUB_ORGANIZATION": chainConfig.Build.GithubOrganization,
		"GITHUB_REPO":         chainConfig.Build.GithubRepo,
		"BUILD_TARGET":        chainConfig.Build.BuildTarget,
		"BINARIES":            binaries,
		"LIBRARIES":           libraries,
		"TARGET_LIBRARIES":    targetLibraries,
		"DIRECTORIES":         directories,
		"PRE_BUILD":           chainConfig.Build.PreBuild,
		"FINAL_IMAGE":         chainConfig.Build.FinalImage,
		"BUILD_ENV":           buildEnv,
		"BUILD_TAGS":          buildTagsEnvVar,
		"BUILD_DIR":           chainConfig.Build.BuildDir,
		"BUILD_TIMESTAMP":     buildTimestamp,
		"GO_VERSION":          baseVer.Version,
		"RACE":                race,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(time.Minute*180))
	defer cancel()

	push := buildCfg.ContainerRegistry != "" && !buildCfg.SkipPush

	if buildCfg.UseBuildKit {
		buildKitOptions := docker.GetDefaultBuildKitOptions()
		buildKitOptions.Address = buildCfg.BuildKitAddr
		supportedPlatforms := chainConfig.Build.Platforms

		if len(supportedPlatforms) > 0 {
			platforms := []string{}
			requestedPlatforms := strings.Split(buildCfg.Platform, ",")
			for _, supportedPlatform := range supportedPlatforms {
				for _, requestedPlatform := range requestedPlatforms {
					if supportedPlatform == requestedPlatform {
						platforms = append(platforms, requestedPlatform)
					}
				}
			}
			if len(platforms) == 0 {
				return fmt.Errorf("no requested platforms are supported for this chain: %s. requested: %s, supported: %s", chainConfig.Build.Name, buildCfg.Platform, strings.Join(supportedPlatforms, ","))
			}
			buildKitOptions.Platform = strings.Join(platforms, ",")
		} else {
			buildKitOptions.Platform = buildCfg.Platform
		}
		buildKitOptions.NoCache = buildCfg.NoCache
		if err := docker.BuildDockerImageWithBuildKit(ctx, reldir, imageTags, push, buildCfg.TarExportPath, buildArgs, buildKitOptions); err != nil {
			return err
		}
	} else {
		if err := docker.BuildDockerImage(ctx, dfilepath, imageTags, push, buildArgs, buildCfg.NoCache); err != nil {
			return err
		}
	}
	return nil
}

// returns queue items, starting with latest for each chain
func (h *HeighlinerBuilder) getNextQueueItem() *ChainNodeDockerBuildConfig {
	h.buildIndexMu.Lock()
	defer h.buildIndexMu.Unlock()
	j := 0
	for i := 0; true; i++ {
		foundForThisIndex := false
		for _, queuedChainBuilds := range h.queue {
			if i < len(queuedChainBuilds.ChainConfigs) {
				if j == h.buildIndex {
					h.buildIndex++
					return &queuedChainBuilds.ChainConfigs[i]
				}
				j++
				foundForThisIndex = true
			}
		}
		if !foundForThisIndex {
			// all done
			return nil
		}
	}
	return nil
}

func (h *HeighlinerBuilder) buildNextImage(wg *sync.WaitGroup) {
	chainConfig := h.getNextQueueItem()
	if chainConfig == nil {
		wg.Done()
		return
	}

	go func() {
		if err := h.buildChainNodeDockerImage(chainConfig); err != nil {
			h.errorsLock.Lock()
			h.errors = append(h.errors, fmt.Errorf("error building docker image for %s from ref: %s - %v\n", chainConfig.Build.Name, chainConfig.Ref, err))
			h.errorsLock.Unlock()
		}
		h.buildNextImage(wg)
	}()
}

func (h *HeighlinerBuilder) queueTmpDirRemoval(tmpDir string, start bool) {
	h.tmpDirMapMu.Lock()
	defer h.tmpDirMapMu.Unlock()
	if start {
		h.tmpDirsToRemove[tmpDir] = true
	} else {
		delete(h.tmpDirsToRemove, tmpDir)
	}
}

// registerSigIntHandler will delete tmp dirs on ctrl+c
func (h *HeighlinerBuilder) registerSigIntHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		h.tmpDirMapMu.Lock()
		defer h.tmpDirMapMu.Unlock()
		for dir := range h.tmpDirsToRemove {
			_ = os.RemoveAll(dir)
		}

		os.Exit(1)
	}()
}

func (h *HeighlinerBuilder) BuildImages() {
	h.registerSigIntHandler()

	wg := new(sync.WaitGroup)
	for i := int16(0); i < h.parallel; i++ {
		wg.Add(1)
		h.buildNextImage(wg)
	}
	wg.Wait()
	if len(h.errors) > 0 {
		for _, err := range h.errors {
			fmt.Println(err)
		}
		panic("Some images failed to build")
	}
}
