package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/auth"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/container"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/execution_experiments"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/overlayfs"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/vfs"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/ci_runner_util"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/vfs_server"
	"github.com/buildbuddy-io/buildbuddy/server/cache/dirtools"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/disk"
	"github.com/buildbuddy-io/buildbuddy/server/util/flag"
	"github.com/buildbuddy-io/buildbuddy/server/util/fspath"
	"github.com/buildbuddy-io/buildbuddy/server/util/gds"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/platform"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/tracing"
	"github.com/gobwas/glob"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"

	ci_runner_bundle "github.com/buildbuddy-io/buildbuddy/enterprise/server/cmd/ci_runner/bundle"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	cli_bundle "github.com/buildbuddy-io/buildbuddy/server/util/bb"
)

var (
	// WorkspaceMarkedForRemovalError is returned from workspace operations
	// whenever Remove was previously called on the workspace.
	WorkspaceMarkedForRemovalError = status.UnavailableError("workspace is marked for removal")

	deleteParallelism = flag.Int("executor.delete_parallelism", 0, "Number of goroutines to use when deleting files.")
	deleteWaitGroup   = errgroup.Group{}
	deleteLimitSet    = sync.Once{}

	vfsVerbose             = flag.Bool("executor.vfs.verbose", false, "Enables verbose logs for VFS operations.")
	vfsVerboseFUSEOps      = flag.Bool("executor.vfs.verbose_fuse", false, "Enables low-level verbose logs in the go-fuse library.")
	vfsLogFUSELatencyStats = flag.Bool("executor.vfs.log_fuse_latency_stats", false, "Enables logging of per-operation latency stats when VFS is unmounted. Implicitly enabled by --executor.vfs.verbose.")
	vfsLogFUSEPerFileStats = flag.Bool("executor.vfs.log_fuse_per_file_stats", false, "Enables tracking and logging of per-file per-operation stats. Logged when VFS is unmounted.")

	reusePoolSize    = flag.Int("executor.workspace_reuse.pool_size", 32, "Maximum number of finished task workspaces to keep for reuse by later tasks with similar input trees, when the executor.workspace_reuse.enabled experiment is on. 0 disables reuse.")
	reuseMaxFileSize = flag.Int64("executor.workspace_reuse.max_file_size_bytes", 1<<20, "Input files larger than this are removed from a workspace before it enters the reuse pool, so that pooled workspaces don't keep large files on disk after the file cache evicts them. This is mostly a hedge against extreme edge cases, since pooled workspaces only hold hard links to file cache entries, and the file cache rarely evicts files that pooled workspaces still use. 0 keeps all files.")
)

func setDeleteLimit() {
	deleteLimitSet.Do(func() {
		limit := *deleteParallelism
		if limit == 0 {
			limit = runtime.GOMAXPROCS(0)
		}
		deleteWaitGroup.SetLimit(limit)
	})
}

// Workspace holds the working tree for an action and keeps track of
// inputs and outputs.
type Workspace struct {
	env     environment.Env
	rootDir string
	overlay *overlayfs.Overlay
	// dirPerms are the permissions set on the workspace root directory as well as
	// any input or output directories created by the executor. It does not affect
	// file permissions.
	dirPerms  fs.FileMode
	task      *repb.ExecutionTask
	dirHelper *dirtools.DirHelper
	Opts      *Opts

	// VFS fields are only set if VFS is enabled.
	// vfs is the FUSE implementation that delegates work to the VFS server.
	vfs *vfs.VFS
	// vfsServer contains most of the smarts of the VFS implementation.
	vfsServer *vfs_server.Server

	// Action input files known to exist in the workspace, as a map of
	// workspace-relative paths to file nodes.
	// TODO: Make sure these files are written read-only
	// to make sure this map accurately reflects the filesystem.
	Inputs map[fspath.Key]*repb.FileNode

	mu          sync.Mutex // protects(removing, treeFetcher)
	removing    bool
	treeFetcher *dirtools.TreeFetcher

	// State for reusing the workspace through Opts.Pool, set when inputs are
	// set up: the input tree the workspace holds, and the group whose tasks
	// may reuse it once it returns to the pool.
	inputRootDigest     *repb.Digest
	inputDirs           map[digest.Key]*repb.Directory
	inputDigestFunction repb.DigestFunction_Value
	groupID             string
	// Each input directory's fingerprint right after inputs were set up, by
	// workspace-relative path. ReturnToPool compares against them to find
	// the directories the task changed.
	dirFingerprints map[string]dirFingerprint
}

// dirFingerprint identifies the state of a directory. The kernel updates a
// directory's ctime whenever an entry is added, removed or renamed, or the
// directory's own attributes change, and userspace can't set ctime back. So if
// a directory's fingerprint still matches, its entries are the same inodes as
// when we recorded it. We also record the mode and owner, so that we recreate a
// directory whose mode or owner the task changed instead of passing it on to
// the next task.
type dirFingerprint struct {
	ino        uint64
	ctimeNanos int64
	mode       uint32
	uid        uint32
	gid        uint32
}

type VFSPrefetchMode string

const (
	VFSPrefetchModeAll  VFSPrefetchMode = platform.VFSPrefetchModeAll
	VFSPrefetchModeNone VFSPrefetchMode = platform.VFSPrefetchModeNone
)

type Opts struct {
	// Preserve specifies whether to preserve all files in the workspace except
	// for output paths.
	Preserve    bool
	CleanInputs string
	// CaseInsensitive specifies whether the root directory (under which the
	// workspace directory is created) is case-insensitive.
	CaseInsensitive bool
	// UseOverlayfs specifies whether the workspace should use overlayfs to
	// allow copy-on-write for workspace inputs.
	UseOverlayfs bool
	// UseVFS specifies whether the workspace should use a FUSE virtual file
	// system to serve CAS artifacts and scratch files.
	UseVFS bool
	// VFSPrefetchMode controls whether VFS inputs are prefetched.
	VFSPrefetchMode VFSPrefetchMode
	// Pool, if set, lets this workspace start from the directory of a
	// finished task with a similar input tree, and keeps this workspace's
	// directory for later tasks when it's removed. Workspaces are only reused
	// within the group of the task's authenticated user.
	Pool *Pool
}

// New creates a new workspace directly under the given parent directory.
func New(env environment.Env, parentDir string, opts *Opts) (*Workspace, error) {
	if opts.VFSPrefetchMode != "" && opts.VFSPrefetchMode != VFSPrefetchModeAll && opts.VFSPrefetchMode != VFSPrefetchModeNone {
		return nil, status.InvalidArgumentErrorf("invalid VFS prefetch mode %q", opts.VFSPrefetchMode)
	}
	dirPerms := fs.FileMode(0777)
	var rootDir string
	maxAttempts := 10
	for i := range maxAttempts {
		rootDir = filepath.Join(parentDir, newRandomBuildDirCandidate())
		if err := os.Mkdir(rootDir, dirPerms); err == nil {
			break
		} else if !os.IsExist(err) {
			return nil, status.UnavailableErrorf("failed to create workspace at %q: %s", rootDir, err)
		}
		// Got unlucky and the random directory name already exists. This should
		// never happen on Unix and rarely on Windows, so just try again.
		if i == maxAttempts-1 {
			return nil, status.UnavailableErrorf("failed to find random dir below %q in %d attempts", rootDir, maxAttempts)
		}
	}
	rootDir, err := maybeCreatePlatformSpecificSubDir(rootDir)
	if err != nil {
		return nil, err
	}

	if opts.UseOverlayfs && opts.UseVFS {
		return nil, status.InvalidArgumentErrorf("Overlayfs and VFS cannot be enabled at the same time")
	}

	var overlay *overlayfs.Overlay
	if opts.UseOverlayfs {
		overlayOpts := overlayfs.Opts{DirPerms: dirPerms}
		var err error
		overlay, err = overlayfs.Convert(context.TODO(), rootDir, overlayOpts)
		if err != nil {
			return nil, status.UnavailableErrorf("failed to create workspace overlayfs at %q: %s", rootDir, err)
		}
	}

	var vfs *vfs.VFS
	var vfsServer *vfs_server.Server
	if opts.UseVFS {
		v, s, err := startVFS(env, rootDir)
		if err != nil {
			return nil, err
		}
		vfs = v
		vfsServer = s
	}

	setDeleteLimit()
	return &Workspace{
		env:       env,
		rootDir:   rootDir,
		overlay:   overlay,
		dirPerms:  dirPerms,
		vfs:       vfs,
		vfsServer: vfsServer,
		Opts:      opts,
		Inputs:    map[fspath.Key]*repb.FileNode{},
	}, nil
}

func startVFS(env environment.Env, path string) (*vfs.VFS, *vfs_server.Server, error) {
	var vfsServer *vfs_server.Server
	scratchDir := path + ".scratch"
	if err := os.Mkdir(scratchDir, 0755); err != nil {
		return nil, nil, status.UnavailableErrorf("could not create FUSE scratch dir: %s", err)
	}

	vfsServer, err := vfs_server.New(env, scratchDir)
	if err != nil {
		return nil, nil, err
	}
	fs := vfs.New(vfs_server.NewDirectClient(vfsServer), path, &vfs.Options{
		EnablePassthrough:   true,
		Verbose:             *vfsVerbose,
		LogFUSEOps:          *vfsVerboseFUSEOps,
		LogFUSELatencyStats: *vfsLogFUSELatencyStats,
		LogFUSEPerFileStats: *vfsLogFUSEPerFileStats,
	})
	if err := fs.Mount(); err != nil {
		return nil, nil, status.UnavailableErrorf("unable to mount VFS at %q: %s", path, err)
	}
	return fs, vfsServer, nil
}

// Path returns the absolute path to the workspace root directory, which should
// be used as the action working directory. If overlayfs is enabled, this will
// be the path where the overlayfs is mounted.
func (ws *Workspace) Path() string {
	return ws.rootDir
}

// inputRoot returns the directory where inputs should be downloaded. For
// overlayfs, this is the overlay lower directory. Otherwise, this behaves the
// same as Path().
func (ws *Workspace) inputRoot() string {
	if ws.overlay == nil {
		return ws.Path()
	}
	return ws.overlay.LowerDir
}

// SetTask sets the next task to be executed within the workspace.
func (ws *Workspace) SetTask(ctx context.Context, task *repb.ExecutionTask) {
	log.CtxDebugf(ctx, "Assigned task %s to workspace at %q", task.GetExecutionId(), ws.rootDir)
	ws.task = task
	cmd := task.GetCommand()
	ws.dirHelper = dirtools.NewDirHelper(filepath.Join(ws.inputRoot(), cmd.GetWorkingDirectory()), cmd, ws.dirPerms)
}

// CommandWorkingDirectory returns the absolute path to the working directory
// for the command to be executed. It assumes that any container executing the
// task will have the workspace mounted at a path identical to its path on the
// executor. For example: /buildbuddy/remotebuilds/abc on the executor's
// filesystem is expected to be mounted to /buildbuddy/remotebuilds/abc in the
// container.
func (ws *Workspace) CommandWorkingDirectory() string {
	dir := ws.Path()
	if wd := ws.task.GetCommand().GetWorkingDirectory(); wd != "" {
		dir = filepath.Join(dir, wd)
	}
	return dir
}

// CreateOutputDirs creates the required output directories for the current
// action.
func (ws *Workspace) CreateOutputDirs() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.removing {
		return WorkspaceMarkedForRemovalError
	}

	return ws.dirHelper.CreateOutputDirs()
}

func (ws *Workspace) prepareVFS(ctx context.Context, layout *container.FileSystemLayout, inputFetcher container.InputFetcher) error {
	if ws.vfs == nil {
		return status.FailedPreconditionError("vfs cannot be null if vfsServer is set")
	}

	invalidatedInodes, err := ws.vfsServer.Prepare(ctx, layout, inputFetcher)
	if err != nil {
		return err
	}
	if err := ws.vfs.PrepareForTask(ctx, ws.task.GetExecutionId(), invalidatedInodes); err != nil {
		return err
	}

	return nil
}

// validateWorkingDirectoryInTree checks that the given working directory path
// exists as a directory in the input tree. This enforces the REAPI spec
// requirement that working_directory "must be a directory which exists in the
// input tree."
func validateWorkingDirectoryInTree(tree *repb.Tree, digestFunction repb.DigestFunction_Value, workingDir string) error {
	_, dirMap, err := dirtools.DirMapFromTree(tree, digestFunction)
	if err != nil {
		return status.WrapError(err, "failed to build directory map from input tree")
	}

	// Walk the tree path segment by segment starting from root.
	current := tree.GetRoot()
	segments := strings.SplitSeq(workingDir, "/")
	for seg := range segments {
		found := false
		for _, dirNode := range current.GetDirectories() {
			if dirNode.GetName() == seg {
				next, ok := dirMap[digest.NewKey(dirNode.GetDigest())]
				if !ok {
					return status.FailedPreconditionErrorf("working_directory %q not found in input tree", workingDir)
				}
				current = next
				found = true
				break
			}
		}
		if !found {
			return status.FailedPreconditionErrorf("working_directory %q does not exist in the input tree", workingDir)
		}
	}
	return nil
}

// DownloadInputs downloads any missing inputs for the current action.
func (ws *Workspace) DownloadInputs(ctx context.Context, layout *container.FileSystemLayout) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.removing {
		return WorkspaceMarkedForRemovalError
	}

	ctx, span := tracing.StartSpan(ctx)
	defer span.End()

	execReq := ws.task.GetExecuteRequest()

	// The REAPI spec requires working_directory to be a directory that
	// already exists in the input tree. Validate this against the tree.
	if wd := ws.task.GetCommand().GetWorkingDirectory(); wd != "" {
		if err := validateWorkingDirectoryInTree(layout.Inputs, execReq.GetDigestFunction(), wd); err != nil {
			return err
		}
	}

	vfsPrefetchMode := ws.Opts.VFSPrefetchMode
	usesVFS := ws.vfs != nil || vfsPrefetchMode != ""
	if vfsPrefetchMode == VFSPrefetchModeNone {
		layout.InputFetcher = nil
		ws.treeFetcher = nil
		if ws.vfs != nil {
			if err := ws.prepareVFS(ctx, layout, nil); err != nil {
				return err
			}
		}
		return nil
	}

	opts := &dirtools.DownloadTreeOpts{CaseInsensitive: ws.Opts.CaseInsensitive}
	if !usesVFS {
		opts.RootDir = ws.inputRoot()
	}
	opts.ChunkedInputFiles = slices.Contains(ws.task.GetExperiments(), "executor.download_inputs_chunked")
	opts.RecordInputFetchMetadata = execution_experiments.RecordInputFetchMetadata.Get(ctx)
	if ws.Opts.Preserve {
		opts.KnownInputs = ws.Inputs
		opts.TrackTransfers = true
	}
	// The runner only sets Opts.Pool for workspaces that we can pool.
	reusable := ws.Opts.Pool != nil
	var reused *reuseStats
	var dirMapDuration time.Duration
	if reusable {
		dirMapStart := time.Now()
		root, dirMap, err := dirtools.DirMapFromTree(layout.Inputs, execReq.GetDigestFunction())
		if err != nil {
			return status.WrapError(err, "build directory map from input tree")
		}
		dirMapDuration = time.Since(dirMapStart)
		ws.inputRootDigest, ws.inputDirs, ws.inputDigestFunction = root, dirMap, execReq.GetDigestFunction()
		ws.groupID = interfaces.AuthAnonymousUser
		if u, err := auth.UserFromTrustedJWT(ctx); err == nil && u.GetGroupID() != "" {
			ws.groupID = u.GetGroupID()
		}
		reused = ws.reusePooledWorkspace(ctx)
		if reused != nil {
			opts.KnownInputs = reused.kept
		}
	}
	tf, err := dirtools.NewTreeFetcher(ctx, ws.env, execReq.GetInstanceName(), execReq.GetDigestFunction(), layout.Inputs, opts)
	if err != nil {
		return status.WrapErrorf(err, "could not create tree fetcher")
	}
	ws.treeFetcher = tf
	if usesVFS {
		layout.InputFetcher = tf
	}

	// Start fetching inputs.
	inputsState, err := tf.Start()
	if err != nil {
		return status.WrapErrorf(err, "could not start tree fetcher")
	}

	// Inform VFS about the layout of the input tree and give it access to the
	// running tree fetcher.
	if usesVFS {
		if ws.vfs != nil {
			if err := ws.prepareVFS(ctx, layout, tf); err != nil {
				return err
			}
		}
	} else {
		// If we're not using FUSE, wait for the input tree to be fully downloaded.
		txInfo, err := ws.treeFetcher.Wait()
		if err != nil {
			return status.WrapError(err, "could not fetch inputs")
		}
		mbps := (float64(txInfo.BytesTransferred) / float64(1e6)) / float64(txInfo.TransferDuration.Seconds())
		span.SetAttributes(attribute.Int64("file_count", txInfo.FileCount))
		span.SetAttributes(attribute.Int64("bytes_transferred", txInfo.BytesTransferred))
		log.CtxDebugf(ctx, "DownloadTree linked %d files in %s, downloaded %d bytes in %s [%2.2f MB/sec]", txInfo.LinkCount, txInfo.LinkDuration, txInfo.BytesTransferred, txInfo.TransferDuration, mbps)
		var fingerprintDuration time.Duration
		if reusable {
			// Now that the inputs are in place, and before the task runs, we
			// record a fingerprint of each input directory. When the workspace
			// returns to the pool, we compare against these, so that we only
			// need to read the directories that the task changed.
			start := time.Now()
			if err := ws.recordDirFingerprints(); err != nil {
				log.CtxWarningf(ctx, "Workspace won't be pooled, because recording directory fingerprints failed: %s", err)
				ws.dirFingerprints = nil
			}
			fingerprintDuration = time.Since(start)
		}
		// Prototype: log a summary for every task, so that runs with and
		// without the reuse pool can be compared.
		ws.logInputSetup(ctx, reused, txInfo, dirMapDuration, fingerprintDuration)
	}

	// Now that the input tree is setup, remove any unwanted inputs.
	// Don't touch any inputs specified by the current action as represented
	// by inputsState.Exist
	if err := ws.CleanInputsIfNecessary(inputsState.Exist); err != nil {
		return err
	}

	// Update the input tracking map to include inputs specified by the action
	// that were not in the workspace yet.
	for relPath, node := range inputsState.NeedFetching {
		ws.Inputs[fspath.NewKey(relPath, ws.Opts.CaseInsensitive)] = node
	}

	return nil
}

func (ws *Workspace) AddRemoteRunnerBinaries(ctx context.Context) error {
	if err := ws.AddCIRunner(ctx); err != nil {
		return err
	}
	// The bb CLI is not embedded on Windows, but the CI runner can still use
	// its embedded Bazelisk binary.
	if len(cli_bundle.CLIBytes) > 0 {
		if err := ws.AddCLI(ctx); err != nil {
			return err
		}
	}
	return nil
}

// AddCLI adds the bb CLI to the workspace root if it doesn't already exist.
func (ws *Workspace) AddCLI(ctx context.Context) error {
	// The CLI binary is not embedded on Windows due to a CGo dependency that isn't supported.
	if len(cli_bundle.CLIBytes) == 0 {
		return status.UnimplementedErrorf("CLI binary not embedded")
	}
	// Don't add CLI if the workspace is backed by FUSE.
	if ws.vfs != nil {
		return status.UnimplementedErrorf("AddCLI not supported on VFS")
	}
	destPath := path.Join(ws.Path(), ci_runner_util.CLIBinaryName)
	exists, err := disk.FileExists(ctx, destPath)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	return os.WriteFile(destPath, cli_bundle.CLIBytes, 0o555)
}

// AddCIRunner adds the BuildBuddy CI runner to the workspace root if it doesn't
// already exist.
func (ws *Workspace) AddCIRunner(ctx context.Context) error {
	// Don't add CI runner if the workspace is backed by FUSE.
	if ws.vfs != nil {
		return status.UnimplementedErrorf("AddCIRunner not supported on VFS")
	}
	destPath := path.Join(ws.Path(), ci_runner_util.ExecutableName)
	exists, err := disk.FileExists(ctx, destPath)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return os.WriteFile(destPath, ci_runner_bundle.CiRunnerBytes, 0o555)
}

func (ws *Workspace) CleanInputsIfNecessary(keep map[fspath.Key]*repb.FileNode) error {
	if ws.Opts.CleanInputs == "" {
		return nil
	}
	inputFilesToCleanUp := make(map[fspath.Key]*repb.FileNode)
	// Curly braces indicate a comma separated list of patterns: https://pkg.go.dev/github.com/gobwas/glob#Compile
	glob, err := glob.Compile(fmt.Sprintf("{%s}", ws.Opts.CleanInputs), os.PathSeparator)
	if err != nil {
		return status.FailedPreconditionErrorf("Invalid glob {%s} used for input cleaning: %s", ws.Opts.CleanInputs, err.Error())
	}
	for path, node := range ws.Inputs {
		// NOTE: the glob is matching against the normalized path key here
		// (which is always lowercase on case-insensitive filesystems), not the
		// path string.
		if ws.Opts.CleanInputs == "*" || glob.Match(path.NormalizedString()) {
			inputFilesToCleanUp[path] = node
		}
	}
	for path := range keep {
		delete(inputFilesToCleanUp, path)
	}
	if len(inputFilesToCleanUp) > 0 {
		for path := range inputFilesToCleanUp {
			if err := os.RemoveAll(filepath.Join(ws.Path(), path.NormalizedString())); err != nil && !os.IsNotExist(err) {
				return status.UnavailableErrorf("Failed to clean inputs: %s", err)
			}
			delete(ws.Inputs, path)
		}
	}
	return nil
}

// UploadOutputs uploads any outputs created by the last executed command
// as well as the command's stdout and stderr.
func (ws *Workspace) UploadOutputs(ctx context.Context, cmd *repb.Command, executeResponse *repb.ExecuteResponse, cmdResult *interfaces.CommandResult) (*dirtools.TransferInfo, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.removing {
		return nil, WorkspaceMarkedForRemovalError
	}

	ctx, span := tracing.StartSpan(ctx)
	defer span.End()

	bsClient := ws.env.GetByteStreamClient()
	instanceName := ws.task.GetExecuteRequest().GetInstanceName()
	digestFunction := ws.task.GetExecuteRequest().GetDigestFunction()

	var txInfo *dirtools.TransferInfo
	var stdoutDigest, stderrDigest *repb.Digest

	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		d, err := cachetools.UploadBlob(egCtx, bsClient, instanceName, digestFunction, bytes.NewReader(cmdResult.Stdout))
		if err != nil {
			return status.UnavailableErrorf("upload stdout: %s", err)
		}
		stdoutDigest = d
		return nil
	})
	eg.Go(func() error {
		d, err := cachetools.UploadBlob(egCtx, bsClient, instanceName, digestFunction, bytes.NewReader(cmdResult.Stderr))
		if err != nil {
			return status.UnavailableErrorf("upload stderr: %s", err)
		}
		stderrDigest = d
		return nil
	})
	eg.Go(func() error {
		if ws.overlay != nil {
			// When overlayfs is enabled, apply the changes from upperdir to
			// lowerdir, since dirHelper's root dir is configured as the
			// lowerdir and it needs to see the output files.
			//
			// Optimization: if recycling is not enabled, then at this point the
			// runner should be removed and cannot affect any files in the
			// workspace anymore, so it is safe to rename the outputs files in
			// upperdir here rather than copying.
			recyclingEnabled := platform.IsRecyclingEnabled(ws.task)
			opts := overlayfs.ApplyOpts{AllowRename: !recyclingEnabled}
			if err := ws.overlay.Apply(egCtx, opts); err != nil {
				return status.WrapError(err, "apply overlay upperdir changes")
			}
		}
		// Don't try to add files to the filecache if VFS is enabled. We use hard links to store data in the file
		// cache and hard links across filesystems are not possible.
		addToFileCache := ws.vfs == nil
		var err error

		txInfo, err = dirtools.UploadTree(
			egCtx,
			ws.env,
			ws.dirHelper,
			instanceName,
			digestFunction,
			filepath.Join(ws.inputRoot(), cmd.GetWorkingDirectory()),
			cmd,
			executeResponse.Result,
			addToFileCache,
			ws.task.GetFastCdc_2020Params(),
		)
		return err
	})
	var logsMu sync.Mutex
	serverLogs := make(map[string]*repb.LogFile, len(cmdResult.AuxiliaryLogs))
	for name, b := range cmdResult.AuxiliaryLogs {
		eg.Go(func() error {
			d, err := cachetools.UploadBlob(egCtx, bsClient, instanceName, digestFunction, bytes.NewReader(b))
			if err != nil {
				log.CtxWarningf(ctx, "Failed to upload auxiliary log %q: %s", name, err)
				return nil
			}
			logsMu.Lock()
			defer logsMu.Unlock()
			serverLogs[name] = &repb.LogFile{
				Digest:        d,
				HumanReadable: true,
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	txInfo.FileCount += 2 // for stdout and stderr
	txInfo.BytesTransferred += int64(len(cmdResult.Stdout) + len(cmdResult.Stderr))
	txInfo.FileCount += int64(len(cmdResult.AuxiliaryLogs))
	for _, b := range cmdResult.AuxiliaryLogs {
		txInfo.BytesTransferred += int64(len(b))
	}
	executeResponse.Result.StdoutDigest = stdoutDigest
	executeResponse.Result.StderrDigest = stderrDigest
	executeResponse.ServerLogs = serverLogs
	span.SetAttributes(attribute.Int64("file_count", txInfo.FileCount))
	span.SetAttributes(attribute.Int64("bytes_transferred", txInfo.BytesTransferred))
	return txInfo, nil
}

func (ws *Workspace) stopVFS(ctx context.Context) error {
	var unmountErr error
	if ws.vfs != nil {
		unmountErr = ws.vfs.Unmount()
	}
	if ws.vfsServer != nil {
		ws.vfsServer.Stop()
		errorChan := make(chan error)
		deleteWaitGroup.Go(func() error {
			errorChan <- disk.ForceRemove(ctx, ws.vfsServer.Path())
			return nil
		})
		if removeErr := <-errorChan; removeErr != nil {
			if unmountErr == nil {
				return status.InternalErrorf("failed to clean up vfs scratch directory: %s", removeErr)
			} else {
				log.CtxWarningf(ctx, "Failed to clean up vfs scratch directory: %s", removeErr)
			}
		}
	}
	return unmountErr
}

func (ws *Workspace) Remove(ctx context.Context) error {
	ws.mu.Lock()
	ws.removing = true
	// No need to keep the lock held while removing; other operations will
	// immediately fail since we've set the removing bit.
	ws.mu.Unlock()

	if ws.overlay != nil {
		return ws.overlay.Remove(ctx)
	}

	if ws.vfs != nil {
		return ws.stopVFS(ctx)
	}

	errorChan := make(chan error)
	// Sometimes removal fails if badly-behaved actions write their
	// directories read-only. Use force-removal to handle these cases.
	deleteWaitGroup.Go(func() error {
		errorChan <- disk.ForceRemove(ctx, ws.rootDir)
		return nil
	})

	if err := <-errorChan; err != nil {
		return status.InternalErrorf("failed to force-remove workspace: %s", err)
	}
	return nil
}

// Size computes the current workspace size in bytes.
func (ws *Workspace) DiskUsageBytes() (int64, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.removing {
		return 0, WorkspaceMarkedForRemovalError
	}

	return disk.DirSize(ws.Path())
}

// ComputeVFSStats returns vfs-specific stats, if vfs is enabled.
//
// When VFS is enabled in the workspace, the VFS implementation keeps track
// of its own IO stats and a call is necessary to propagate these stats to the
// action results.
func (ws *Workspace) ComputeVFSStats() *repb.VfsStats {
	if ws.vfsServer == nil {
		return nil
	}
	return ws.vfsServer.ComputeStats()
}

// VFSError returns the first input download error exposed to the current task,
// or nil if VFS is disabled or no download failed after retries.
func (ws *Workspace) VFSError() error {
	if ws.vfsServer == nil {
		return nil
	}
	return ws.vfsServer.TaskError()
}

// TaskFinished informs the workspace that task execution is done.
// Returns the transfer stats.
func (ws *Workspace) TaskFinished() (*dirtools.TransferInfo, error) {
	ws.mu.Lock()
	tf := ws.treeFetcher
	ws.mu.Unlock()
	if tf == nil {
		if ws.Opts.VFSPrefetchMode == VFSPrefetchModeNone {
			return &dirtools.TransferInfo{}, nil
		}
		return nil, status.FailedPreconditionError("tree fetcher not set")
	}
	// TODO(vadim): cancel unfinished transfers instead of waiting for them
	txInfo, err := tf.Wait()
	ws.mu.Lock()
	ws.treeFetcher = nil
	ws.mu.Unlock()
	return txInfo, err
}

// Clean removes files and directories in the workspace which are not preserved
// according to the workspace options.
func (ws *Workspace) Clean() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.removing {
		return WorkspaceMarkedForRemovalError
	}

	// No task is currently assigned; nothing to clean.
	if ws.task == nil {
		return nil
	}

	// If preserving the workspace, only remove outputs and leave other files
	// as-is.
	if ws.Opts.Preserve {
		cmd := ws.task.GetCommand()
		outputPaths := slices.Concat(cmd.GetOutputFiles(), cmd.GetOutputDirectories(), cmd.GetOutputPaths())
		workingDir := cmd.GetWorkingDirectory()
		for _, outputPath := range outputPaths {
			// Resolve output path relative to working directory, since
			// output paths in the command are relative to working_directory
			// but ws.Inputs and the filesystem are relative to the
			// workspace root.
			fullOutputPath := filepath.Join(workingDir, outputPath)
			if err := os.RemoveAll(filepath.Join(ws.Path(), fullOutputPath)); err != nil && !os.IsNotExist(err) {
				return status.UnavailableErrorf("Failed to clean workspace: %s", err)
			}
			// If the output path was a directory, we need to delete any known
			// input files which lived under that output directory.
			for inputKey := range ws.Inputs {
				if fspath.IsParent(fullOutputPath, inputKey.NormalizedString(), ws.Opts.CaseInsensitive) {
					delete(ws.Inputs, inputKey)
				}
			}
			// In case this output path previously pointed to an input file,
			// delete it from the inputs index (this should be pretty uncommon).
			delete(ws.Inputs, fspath.NewKey(fullOutputPath, ws.Opts.CaseInsensitive))
		}
		return nil
	}

	if err := removeChildren(ws.Path()); err != nil {
		return status.UnavailableErrorf("Failed to clean workspace: %s", err)
	}
	return nil
}

func removeChildren(dirPath string) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dirPath, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Pool holds the workspaces of finished tasks, so that a later task with
// a similar input tree can start from one of them. For the later task, the
// executor then only creates and removes the entries that differ, instead of
// hard-linking its whole input tree into an empty directory and deleting all
// of it afterwards, which costs a journaled metadata update per file.
//
// The runner calls ReturnToPool after removing the task's container.
// ReturnToPool removes everything from the workspace that isn't one of the
// task's unchanged inputs, then moves the workspace into the pool. Directories
// whose inode, ctime, mode and owner still match what we recorded after input
// setup can't have had entries added, removed or replaced, so we skip them. In
// the other directories, we compare each entry with the input tree and remove
// entries that aren't inputs, such as outputs, and input files that aren't the
// file cache's copy. Only the executor can reach pooled directories, so when
// we reuse a pooled workspace for a later task, we don't look at its files
// again, and only remove the inputs that the later task's tree doesn't have.
type Pool struct {
	dir string

	mu sync.Mutex // protects all of the fields below
	// Pooled workspaces, in no particular order.
	entries []*pooledWorkspace
	// Chooses which entry to evict.
	eviction *gds.Pool[*pooledWorkspace]
}

type pooledWorkspace struct {
	groupID        string
	path           string
	digestFunction repb.DigestFunction_Value
	rootDigest     *repb.Digest
	dirMap         map[digest.Key]*repb.Directory
	totalFiles     int
	// Paths of input entries that we removed when the workspace returned to
	// the pool, because its task changed them. A directory path covers its
	// whole subtree.
	missing map[string]bool
	// The ancestors of every missing path. When syncing, we use them to tell
	// which subtrees with unchanged digests have holes in them.
	missingAncestors map[string]bool
}

// NewPool returns a pool that keeps workspaces under buildRoot, or nil if
// workspace reuse is disabled.
func NewPool(buildRoot string) (*Pool, error) {
	if *reusePoolSize <= 0 {
		return nil, nil
	}
	dir := filepath.Join(buildRoot, ".workspace-reuse-pool")
	// Workspaces pooled by a previous executor process aren't tracked, so
	// start with an empty pool.
	if err := disk.ForceRemove(context.Background(), dir); err != nil {
		return nil, fmt.Errorf("clear workspace reuse pool: %w", err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create workspace reuse pool: %w", err)
	}
	return &Pool{dir: dir, eviction: gds.New[*pooledWorkspace]()}, nil
}

// countTreeFiles returns the number of files in the subtree rooted at d,
// counting a repeated subdirectory once per place it appears.
func countTreeFiles(d *repb.Digest, dirMap map[digest.Key]*repb.Directory, memo map[digest.Key]int) int {
	k := digest.NewKey(d)
	if n, ok := memo[k]; ok {
		return n
	}
	dir := dirMap[k]
	n := len(dir.GetFiles())
	for _, child := range dir.GetDirectories() {
		n += countTreeFiles(child.GetDigest(), dirMap, memo)
	}
	memo[k] = n
	return n
}

// sharedFiles returns the number of files that a workspace holding the old
// tree already has in the right place for the new tree.
func sharedFiles(oldDigest *repb.Digest, oldDirs map[digest.Key]*repb.Directory, newDigest *repb.Digest, newDirs map[digest.Key]*repb.Directory, sizes map[digest.Key]int) int {
	oldKey, newKey := digest.NewKey(oldDigest), digest.NewKey(newDigest)
	if oldKey == newKey {
		return countTreeFiles(newDigest, newDirs, sizes)
	}
	oldDir, newDir := oldDirs[oldKey], newDirs[newKey]
	oldFiles := make(map[string]*repb.FileNode, len(oldDir.GetFiles()))
	for _, f := range oldDir.GetFiles() {
		oldFiles[f.GetName()] = f
	}
	n := 0
	for _, f := range newDir.GetFiles() {
		if o := oldFiles[f.GetName()]; o != nil && digest.NewKey(o.GetDigest()) == digest.NewKey(f.GetDigest()) && o.GetIsExecutable() == f.GetIsExecutable() {
			n++
		}
	}
	oldChildren := make(map[string]*repb.Digest, len(oldDir.GetDirectories()))
	for _, c := range oldDir.GetDirectories() {
		oldChildren[c.GetName()] = c.GetDigest()
	}
	for _, c := range newDir.GetDirectories() {
		if o := oldChildren[c.GetName()]; o != nil {
			n += sharedFiles(o, oldDirs, c.GetDigest(), newDirs, sizes)
		}
	}
	return n
}

// take removes and returns the pooled workspace that saves the most work for
// the given input tree, or nil if starting from an empty workspace is cheaper.
func (p *Pool) take(groupID string, newRoot *repb.Digest, newDirs map[digest.Key]*repb.Directory, digestFunction repb.DigestFunction_Value) *pooledWorkspace {
	sizes := map[digest.Key]int{}
	p.mu.Lock()
	defer p.mu.Unlock()
	best, bestGain := -1, 0
	for i, e := range p.entries {
		if e.groupID != groupID || e.digestFunction != digestFunction {
			continue
		}
		// To reuse a workspace, we remove the old files that don't match and
		// link the new ones that are missing. So we only reuse it if we can
		// keep more than half of its old files, since otherwise starting from
		// an empty workspace takes fewer filesystem operations.
		gain := 2*sharedFiles(e.rootDigest, e.dirMap, newRoot, newDirs, sizes) - e.totalFiles
		if gain > bestGain {
			best, bestGain = i, gain
		}
	}
	if best < 0 {
		return nil
	}
	e := p.entries[best]
	p.entries = slices.Delete(p.entries, best, best+1)
	p.eviction.Remove(e)
	return e
}

// add adds a workspace to the pool, then evicts workspaces until the pool is
// within its size limit, which may evict the new one. It returns the number of
// workspaces evicted, and the number of pooled workspaces afterwards.
func (p *Pool) add(e *pooledWorkspace) (evicted int, workspaces int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, e)
	// We value a workspace by its file count, since reusing it saves up to
	// that many links.
	p.eviction.Add(e, int64(e.totalFiles))
	for len(p.entries) > *reusePoolSize {
		v, ok := p.eviction.Evict()
		if !ok {
			break
		}
		p.entries = slices.DeleteFunc(p.entries, func(c *pooledWorkspace) bool { return c == v })
		evicted++
		log.Infof("Evicted pooled workspace: files=%d new=%t", v.totalFiles, v == e)
		deleteWaitGroup.Go(func() error {
			if err := disk.ForceRemove(context.Background(), v.path); err != nil {
				log.Warningf("Failed to remove evicted workspace %q: %s", v.path, err)
			}
			return nil
		})
	}
	return evicted, len(p.entries)
}

// recordDirFingerprints records the fingerprint of every input directory, so
// that ReturnToPool can tell which directories the task changed.
func (ws *Workspace) recordDirFingerprints() error {
	var dirs []string
	var walk func(rel string, d *repb.Digest)
	walk = func(rel string, d *repb.Digest) {
		dirs = append(dirs, rel)
		for _, child := range ws.inputDirs[digest.NewKey(d)].GetDirectories() {
			walk(path.Join(rel, child.GetName()), child.GetDigest())
		}
	}
	walk("", ws.inputRootDigest)
	// This runs before the task, so we lstat the directories in parallel.
	fps := make([]dirFingerprint, len(dirs))
	var eg errgroup.Group
	eg.SetLimit(16)
	for i, rel := range dirs {
		eg.Go(func() error {
			fp, isDir, err := lstatDir(filepath.Join(ws.inputRoot(), rel))
			if err != nil {
				return err
			}
			if !isDir {
				return fmt.Errorf("input directory %q is not a directory", rel)
			}
			fps[i] = fp
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	fingerprints := make(map[string]dirFingerprint, len(dirs))
	var latestCtime int64
	for i, rel := range dirs {
		fingerprints[rel] = fps[i]
		latestCtime = max(latestCtime, fps[i].ctimeNanos)
	}
	if err := waitForTimestampTick(latestCtime); err != nil {
		return err
	}
	ws.dirFingerprints = fingerprints
	return nil
}

// outputParentDirs returns the workspace-relative paths of the directories
// where CreateOutputDirs may have created entries before the input directory
// fingerprints were recorded. These are the proper ancestors of the output
// directories and of the parents of output paths, matching
// dirtools.NewDirHelper.
func outputParentDirs(cmd *repb.Command) map[string]bool {
	outputPaths := cmd.GetOutputPaths()
	if len(outputPaths) == 0 {
		outputPaths = slices.Concat(cmd.GetOutputFiles(), cmd.GetOutputDirectories())
	}
	var created []string
	for _, p := range outputPaths {
		created = append(created, path.Dir(path.Join(cmd.GetWorkingDirectory(), p)))
	}
	for _, p := range cmd.GetOutputDirectories() {
		created = append(created, path.Join(cmd.GetWorkingDirectory(), p))
	}
	parents := map[string]bool{}
	for _, dir := range created {
		for dir != "." && dir != "/" {
			dir = path.Dir(dir)
			if dir == "." {
				parents[""] = true
			} else {
				parents[dir] = true
			}
		}
	}
	return parents
}

// returnCheck holds what ReturnToPool found when checking a finished
// task's workspace against its input tree.
type returnCheck struct {
	outputParents map[string]bool
	missing       map[string]bool

	dirs, changedDirs, readDirs, checkedFiles, removedOther int
	rejected                                                map[string]int
	rejectedExamples, otherExamples                         []string
	otherByPath                                             map[string]int

	largeFiles int
}

// reject records that the input at rel was removed, and why.
func (c *returnCheck) reject(reason, rel string) {
	c.missing[rel] = true
	c.rejected[reason]++
	if len(c.rejectedExamples) < 5 {
		c.rejectedExamples = append(c.rejectedExamples, reason+": "+rel)
	}
}

// ReturnToPool checks the workspace against the input tree that was set
// up in it, removes everything that isn't an unchanged input, and moves it
// into the reuse pool. It must be called after the container using the
// workspace has been removed, and before Remove, which then finds nothing to
// delete. If the workspace can't be pooled, it's left for Remove.
func (ws *Workspace) ReturnToPool(ctx context.Context) {
	pool := ws.Opts.Pool
	if pool == nil || ws.dirFingerprints == nil || ws.overlay != nil || ws.vfs != nil {
		return
	}
	ws.mu.Lock()
	removing := ws.removing
	ws.mu.Unlock()
	if removing {
		return
	}
	start := time.Now()
	c := &returnCheck{
		outputParents: outputParentDirs(ws.task.GetCommand()),
		missing:       map[string]bool{},
		rejected:      map[string]int{},
		otherByPath:   map[string]int{},
	}
	// We check the workspace before renaming it, because renaming the root
	// directory changes its ctime.
	ok, err := ws.checkReturnedDir(ctx, "", ws.inputRootDigest, c)
	if err != nil {
		log.CtxWarningf(ctx, "Not pooling workspace: %s", err)
		return
	}
	if !ok {
		log.CtxWarningf(ctx, "Not pooling workspace, because the task replaced its root directory")
		return
	}
	pooledPath := filepath.Join(pool.dir, newRandomBuildDirCandidate())
	if err := os.Rename(ws.rootDir, pooledPath); err != nil {
		log.CtxWarningf(ctx, "Not pooling workspace: %s", err)
		return
	}
	missingAncestors := map[string]bool{}
	for rel := range c.missing {
		for rel != "" {
			rel = path.Dir(rel)
			if rel == "." {
				rel = ""
			}
			missingAncestors[rel] = true
		}
	}
	evicted, poolWorkspaces := pool.add(&pooledWorkspace{
		groupID:          ws.groupID,
		path:             pooledPath,
		digestFunction:   ws.inputDigestFunction,
		rootDigest:       ws.inputRootDigest,
		dirMap:           ws.inputDirs,
		totalFiles:       countTreeFiles(ws.inputRootDigest, ws.inputDirs, map[digest.Key]int{}),
		missing:          c.missing,
		missingAncestors: missingAncestors,
	})
	log.CtxInfof(ctx, "Workspace returned to reuse pool: dirs=%d changed_dirs=%d read_dirs=%d checked_files=%d removed_inputs=%d removed_non_inputs=%d large_files_removed=%d evicted=%d pool_workspaces=%d duration=%s rejected=[%s] rejected_examples=%q non_input_examples=%q non_inputs_by_path=[%s]",
		c.dirs, c.changedDirs, c.readDirs, c.checkedFiles, len(c.missing), c.removedOther, c.largeFiles, evicted, poolWorkspaces, time.Since(start),
		topCounts(c.rejected, 5), c.rejectedExamples, c.otherExamples, topCounts(c.otherByPath, 8))
}

// checkReturnedDir makes the directory at rel hold only the entries of the
// input directory d that the task didn't change, removing everything else. It
// returns false if the task removed or replaced the directory itself, or
// changed its mode or owner, in which case the caller removes it.
func (ws *Workspace) checkReturnedDir(ctx context.Context, rel string, d *repb.Digest, c *returnCheck) (bool, error) {
	full := filepath.Join(ws.inputRoot(), rel)
	fp, isDir, err := lstatDir(full)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	want, ok := ws.dirFingerprints[rel]
	if !ok || !isDir || fp.ino != want.ino || fp.mode != want.mode || fp.uid != want.uid || fp.gid != want.gid {
		return false, nil
	}
	c.dirs++
	dir := ws.inputDirs[digest.NewKey(d)]
	changed := fp != want
	if changed {
		c.changedDirs++
	}
	// A directory with an unchanged fingerprint holds the same entries as
	// after setup, which are the inputs plus any that CreateOutputDirs
	// created. So we only read directories that changed, or that
	// CreateOutputDirs may have added to.
	if changed || c.outputParents[rel] {
		if err := ws.checkReturnedEntries(ctx, rel, dir, changed, c); err != nil {
			return false, err
		}
	}
	if *reuseMaxFileSize > 0 {
		for _, f := range dir.GetFiles() {
			fileRel := path.Join(rel, f.GetName())
			if f.GetDigest().GetSizeBytes() <= *reuseMaxFileSize || c.missing[fileRel] {
				continue
			}
			if err := os.Remove(filepath.Join(full, f.GetName())); err != nil && !os.IsNotExist(err) {
				return false, err
			}
			c.missing[fileRel] = true
			c.largeFiles++
		}
	}
	for _, child := range dir.GetDirectories() {
		childRel := path.Join(rel, child.GetName())
		if c.missing[childRel] {
			continue
		}
		ok, err := ws.checkReturnedDir(ctx, childRel, child.GetDigest(), c)
		if err != nil {
			return false, err
		}
		if !ok {
			c.reject("directory replaced", childRel)
			if err := disk.ForceRemove(ctx, filepath.Join(full, child.GetName())); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// checkReturnedEntries removes the entries of the directory at rel that aren't
// in the input directory dir, such as outputs. If the directory changed since
// setup, it also removes the inputs in it that the task replaced.
// Subdirectories are left to checkReturnedDir.
func (ws *Workspace) checkReturnedEntries(ctx context.Context, rel string, dir *repb.Directory, changed bool, c *returnCheck) error {
	c.readDirs++
	full := filepath.Join(ws.inputRoot(), rel)
	entries, err := os.ReadDir(full)
	if err != nil {
		return err
	}
	files := make(map[string]*repb.FileNode, len(dir.GetFiles()))
	for _, f := range dir.GetFiles() {
		files[f.GetName()] = f
	}
	symlinks := make(map[string]string, len(dir.GetSymlinks()))
	for _, s := range dir.GetSymlinks() {
		// The tree fetcher creates empty symlink targets as ".".
		target := s.GetTarget()
		if target == "" {
			target = "."
		}
		symlinks[s.GetName()] = target
	}
	children := make(map[string]*repb.Digest, len(dir.GetDirectories()))
	for _, child := range dir.GetDirectories() {
		children[child.GetName()] = child.GetDigest()
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		entryRel := path.Join(rel, name)
		entryFull := filepath.Join(full, name)
		present[name] = true
		target, isSymlink := symlinks[name]
		reason := ""
		switch {
		case children[name] != nil:
			if !entry.IsDir() {
				reason = "directory replaced"
			}
		case files[name] != nil:
			if !entry.Type().IsRegular() {
				reason = "file replaced"
			} else if changed {
				c.checkedFiles++
				reason = ws.cachedCopyMismatch(ctx, entryFull, files[name])
			}
		case isSymlink:
			if entry.Type()&fs.ModeSymlink == 0 {
				reason = "symlink replaced"
			} else if changed {
				if got, err := os.Readlink(entryFull); err != nil || got != target {
					reason = "symlink replaced"
				}
			}
		default:
			c.removedOther++
			c.otherByPath[insightPrefix(entryRel)]++
			if len(c.otherExamples) < 20 && !strings.HasPrefix(entryRel, "bazel-out/k8-fastbuild/testlogs/") {
				c.otherExamples = append(c.otherExamples, entryRel)
			}
			if err := disk.ForceRemove(ctx, entryFull); err != nil {
				return err
			}
			continue
		}
		if reason == "" {
			continue
		}
		c.reject(reason, entryRel)
		if err := disk.ForceRemove(ctx, entryFull); err != nil {
			return err
		}
	}
	for name := range files {
		if !present[name] {
			c.reject("file deleted", path.Join(rel, name))
		}
	}
	for name := range symlinks {
		if !present[name] {
			c.reject("symlink deleted", path.Join(rel, name))
		}
	}
	for name := range children {
		if !present[name] {
			c.reject("directory deleted", path.Join(rel, name))
		}
	}
	return nil
}

// cachedCopyMismatch returns "" if the file at fullPath is the file cache's
// copy of node, or else why it isn't.
func (ws *Workspace) cachedCopyMismatch(ctx context.Context, fullPath string, node *repb.FileNode) string {
	fc := ws.env.GetFileCache()
	if fc == nil {
		return "no file cache"
	}
	f, err := fc.Open(ctx, node)
	if err != nil {
		return "not in file cache"
	}
	defer f.Close()
	cached, err := f.Stat()
	if err != nil {
		return "stat cached file"
	}
	existing, err := os.Lstat(fullPath)
	if err != nil {
		return "stat workspace file"
	}
	if !os.SameFile(cached, existing) {
		return "different file"
	}
	return ""
}

// reuseStats describes how a task's workspace was set up from a pooled one.
type reuseStats struct {
	// Input files already in place, which the tree fetcher can skip.
	kept         map[fspath.Key]*repb.FileNode
	keptByPath   map[string]int
	keptSymlinks int

	removedFiles, removedSymlinks, removedDirs int
	scoreDuration, duration                    time.Duration
}

// reusePooledWorkspace replaces this workspace's directory with the pooled
// workspace that best matches the input tree, and removes every entry in it
// that the input tree doesn't have. The new directory only holds the empty
// output directories that PrepareForTask created, which are recreated
// afterwards. It returns nil, leaving a workspace with just those directories,
// if no pooled workspace is worth reusing.
func (ws *Workspace) reusePooledWorkspace(ctx context.Context) *reuseStats {
	start := time.Now()
	e := ws.Opts.Pool.take(ws.groupID, ws.inputRootDigest, ws.inputDirs, ws.inputDigestFunction)
	if e == nil {
		return nil
	}
	scoreDuration := time.Since(start)
	// resetToEmpty recreates the directory as PrepareForTask left it, for
	// starting over without the pooled workspace.
	resetToEmpty := func() {
		if err := disk.ForceRemove(ctx, ws.rootDir); err != nil {
			log.CtxErrorf(ctx, "Failed to clear workspace: %s", err)
		}
		if err := os.Mkdir(ws.rootDir, ws.dirPerms); err != nil {
			log.CtxErrorf(ctx, "Failed to recreate workspace directory: %s", err)
		}
		if err := ws.dirHelper.CreateOutputDirs(); err != nil {
			log.CtxErrorf(ctx, "Failed to recreate output directories: %s", err)
		}
	}
	if err := disk.ForceRemove(ctx, ws.rootDir); err != nil {
		log.CtxWarningf(ctx, "Not reusing pooled workspace: %s", err)
		deleteWaitGroup.Go(func() error { return disk.ForceRemove(context.Background(), e.path) })
		resetToEmpty()
		return nil
	}
	if err := os.Rename(e.path, ws.rootDir); err != nil {
		log.CtxWarningf(ctx, "Not reusing pooled workspace: %s", err)
		deleteWaitGroup.Go(func() error { return disk.ForceRemove(context.Background(), e.path) })
		resetToEmpty()
		return nil
	}
	stats := &reuseStats{
		kept:          map[fspath.Key]*repb.FileNode{},
		keptByPath:    map[string]int{},
		scoreDuration: scoreDuration,
	}
	err := ws.syncDir(ctx, e, "", e.rootDigest, ws.inputRootDigest, stats)
	if err == nil {
		err = ws.dirHelper.CreateOutputDirs()
	}
	if err != nil {
		// Start over from an empty workspace rather than trusting a partly
		// synced one.
		log.CtxWarningf(ctx, "Not reusing pooled workspace: %s", err)
		resetToEmpty()
		return nil
	}
	stats.duration = time.Since(start)
	ws.markKeptFilesUsed(ctx, stats.kept)
	return stats
}

// markKeptFilesUsed looks up the given input files in the file cache in the
// background, which marks them as recently used. We don't link reused inputs
// from the file cache, since they're already in the workspace, so without this
// the file cache would evict files that tasks keep reusing through pooled
// workspaces.
func (ws *Workspace) markKeptFilesUsed(ctx context.Context, kept map[fspath.Key]*repb.FileNode) {
	fc := ws.env.GetFileCache()
	if fc == nil {
		return
	}
	// The same file often appears at several paths, so look each one up once.
	// File cache entries are keyed by digest and executable bit.
	type entryKey struct {
		digest     digest.Key
		executable bool
	}
	nodes := make(map[entryKey]*repb.FileNode, len(kept))
	for _, n := range kept {
		nodes[entryKey{digest.NewKey(n.GetDigest()), n.GetIsExecutable()}] = n
	}
	// We don't wait for the lookups before running the task, so we run them
	// with a context that the end of the task doesn't cancel. The file cache
	// needs the context's values to find the group's entries.
	ctx = context.WithoutCancel(ctx)
	go func() {
		for _, n := range nodes {
			fc.ContainsFile(ctx, n)
		}
	}()
}

// syncDir removes the entries of the pooled directory oldDigest at rel that
// the new input directory newDigest doesn't have, and adds the input files
// that are already in place to stats.kept. It only reads the trees, apart
// from the removals.
func (ws *Workspace) syncDir(ctx context.Context, e *pooledWorkspace, rel string, oldDigest, newDigest *repb.Digest, stats *reuseStats) error {
	newKey := digest.NewKey(newDigest)
	if digest.NewKey(oldDigest) == newKey && !e.missingAncestors[rel] {
		ws.keepSubtree(rel, newDigest, stats)
		return nil
	}
	oldDir, newDir := e.dirMap[digest.NewKey(oldDigest)], ws.inputDirs[newKey]
	full := filepath.Join(ws.inputRoot(), rel)
	newFiles := make(map[string]*repb.FileNode, len(newDir.GetFiles()))
	for _, f := range newDir.GetFiles() {
		newFiles[f.GetName()] = f
	}
	newSymlinks := make(map[string]string, len(newDir.GetSymlinks()))
	for _, s := range newDir.GetSymlinks() {
		newSymlinks[s.GetName()] = s.GetTarget()
	}
	newChildren := make(map[string]*repb.Digest, len(newDir.GetDirectories()))
	for _, child := range newDir.GetDirectories() {
		newChildren[child.GetName()] = child.GetDigest()
	}
	for _, f := range oldDir.GetFiles() {
		fileRel := path.Join(rel, f.GetName())
		if e.missing[fileRel] {
			continue
		}
		if n := newFiles[f.GetName()]; n != nil && digest.NewKey(n.GetDigest()) == digest.NewKey(f.GetDigest()) && n.GetIsExecutable() == f.GetIsExecutable() {
			stats.kept[fspath.NewKey(fileRel, ws.Opts.CaseInsensitive)] = n
			stats.keptByPath[insightPrefix(fileRel)]++
			continue
		}
		if err := os.Remove(filepath.Join(full, f.GetName())); err != nil {
			return err
		}
		stats.removedFiles++
	}
	for _, s := range oldDir.GetSymlinks() {
		if e.missing[path.Join(rel, s.GetName())] {
			continue
		}
		// The tree fetcher keeps existing symlinks whose target matches.
		if target, ok := newSymlinks[s.GetName()]; ok && target == s.GetTarget() {
			stats.keptSymlinks++
			continue
		}
		if err := os.Remove(filepath.Join(full, s.GetName())); err != nil {
			return err
		}
		stats.removedSymlinks++
	}
	for _, child := range oldDir.GetDirectories() {
		childRel := path.Join(rel, child.GetName())
		if e.missing[childRel] {
			continue
		}
		if newChild := newChildren[child.GetName()]; newChild != nil {
			if err := ws.syncDir(ctx, e, childRel, child.GetDigest(), newChild, stats); err != nil {
				return err
			}
			continue
		}
		if err := disk.ForceRemove(ctx, filepath.Join(full, child.GetName())); err != nil {
			return err
		}
		stats.removedDirs++
	}
	return nil
}

// keepSubtree adds every file in the input directory d at rel to stats.kept.
func (ws *Workspace) keepSubtree(rel string, d *repb.Digest, stats *reuseStats) {
	dir := ws.inputDirs[digest.NewKey(d)]
	for _, f := range dir.GetFiles() {
		fileRel := path.Join(rel, f.GetName())
		stats.kept[fspath.NewKey(fileRel, ws.Opts.CaseInsensitive)] = f
		stats.keptByPath[insightPrefix(fileRel)]++
	}
	stats.keptSymlinks += len(dir.GetSymlinks())
	for _, child := range dir.GetDirectories() {
		ws.keepSubtree(path.Join(rel, child.GetName()), child.GetDigest(), stats)
	}
}

// insightPrefix groups a workspace-relative path for reporting. Paths under a
// node_modules directory are grouped by the package directly under it, or by
// the package store entry for rules_js; other paths by their first three
// directories.
func insightPrefix(rel string) string {
	parts := strings.Split(rel, "/")
	dirs := parts[:len(parts)-1]
	for i, p := range dirs {
		if p != "node_modules" || i+1 >= len(dirs) {
			continue
		}
		end := i + 2
		if dirs[i+1] == ".aspect_rules_js" && i+2 < len(dirs) {
			end = i + 3
		}
		return strings.Join(parts[:end], "/")
	}
	return strings.Join(dirs[:min(3, len(dirs))], "/")
}

func topCounts(counts map[string]int, n int) string {
	keys := slices.Collect(maps.Keys(counts))
	slices.SortFunc(keys, func(a, b string) int { return counts[b] - counts[a] })
	var b strings.Builder
	for i, k := range keys[:min(n, len(keys))] {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d", k, counts[k])
	}
	return b.String()
}

// logInputSetup logs how the task's inputs were set up, for comparing runs
// with and without workspace reuse.
func (ws *Workspace) logInputSetup(ctx context.Context, reused *reuseStats, txInfo *dirtools.TransferInfo, dirMapDuration, fingerprintDuration time.Duration) {
	if reused == nil {
		log.CtxInfof(ctx, "Workspace input setup: reused=false linked=%d downloaded=%d fetch_duration=%s dir_map_duration=%s fingerprint_duration=%s", txInfo.LinkCount, txInfo.FileCount, txInfo.TransferDuration, dirMapDuration, fingerprintDuration)
		return
	}
	log.CtxInfof(ctx, "Workspace input setup: reused=true kept=%d linked=%d downloaded=%d removed_files=%d removed_symlinks=%d removed_dirs=%d kept_symlinks=%d fetch_duration=%s dir_map_duration=%s score_duration=%s reuse_duration=%s fingerprint_duration=%s kept_by_path=[%s]",
		len(reused.kept), txInfo.LinkCount, txInfo.FileCount, reused.removedFiles, reused.removedSymlinks, reused.removedDirs, reused.keptSymlinks,
		txInfo.TransferDuration, dirMapDuration, reused.scoreDuration, reused.duration, fingerprintDuration, topCounts(reused.keptByPath, 8))
}
