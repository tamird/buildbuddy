package firecracker_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/containers/firecracker"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/oci"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testfs"
	"github.com/stretchr/testify/require"

	fcpb "github.com/buildbuddy-io/buildbuddy/proto/firecracker"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
)

var netemKernelRlocationpath string

// TestFirecrackerNetem boots the source-built kernel rather than a published
// guest artifact. This qualifier is separate from the three-config product fix.
func TestFirecrackerNetem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := getTestEnv(ctx, t, envOpts{})
	rootDir := testfs.MakeTempDir(t)
	workDir := testfs.MakeDirAll(t, rootDir, "work")
	kernelPath, err := runfiles.Rlocation(netemKernelRlocationpath)
	require.NoError(t, err)
	kernel, err := os.ReadFile(kernelPath)
	require.NoError(t, err)
	kernelHash := fmt.Sprintf("%x", sha256.Sum256(kernel))
	t.Logf("NETEM_KERNEL sha256=%s bytes=%d path=%s", kernelHash, len(kernel), kernelPath)
	cfg := getExecutorConfig(t)
	cfg.GuestKernelImagePath = kernelPath
	cfg.GuestKernelVersion = kernelHash
	cfg.GuestKernelImagePath6_18 = kernelPath
	cfg.GuestKernelVersion6_18 = kernelHash
	opts := firecracker.ContainerOpts{
		ContainerImage:         imageWithDockerV28Installed,
		ActionWorkingDirectory: workDir,
		VMConfiguration: &fcpb.VMConfiguration{
			NumCpus:           1,
			MemSizeMb:         2500,
			NetworkMode:       fcpb.NetworkMode_NETWORK_MODE_OFF,
			ScratchDiskSizeMb: 100,
		},
		ExecutorConfig: cfg,
	}
	c, err := firecracker.NewContainer(ctx, env, &repb.ExecutionTask{}, opts)
	require.NoError(t, err)
	// Run owns VM teardown even when the guest command fails.
	res := c.Run(ctx, &repb.Command{Arguments: []string{"sh", "-ec", `
		uname -r
		command -v ip
		command -v tc
		ip -Version
		tc -Version
		ip link add netem-left type veth peer name netem-right
		cleanup() {
			rc=$?
			trap - EXIT
			ip link del netem-left || rc=1
			exit "$rc"
		}
		trap cleanup EXIT
		ip link set netem-left up
		ip link set netem-right up
		tc qdisc add dev netem-left root netem limit 100 delay 50ms rate 20mbit
		printf 'NETEM_ADDED\n'
		tc -s qdisc show dev netem-left
		tc qdisc change dev netem-left root netem limit 100 delay 5ms rate 20mbit
		printf 'NETEM_CHANGED\n'
		tc -s qdisc show dev netem-left
		tc qdisc del dev netem-left root
		ip link del netem-left
		trap - EXIT
		if ip link show netem-left; then exit 1; fi
		printf 'NETEM_CLEANED\n'
	`}}, workDir, oci.Credentials{})
	t.Logf("guest stdout:\n%s\nguest stderr:\n%s", res.Stdout, res.Stderr)
	require.NoError(t, res.Error)
	require.Equal(t, 0, res.ExitCode)
	require.Regexp(t, `(?m)^6\.18\.57(?:\S*)$`, string(res.Stdout))
	require.Regexp(t, `(?s)NETEM_ADDED\nqdisc netem .*limit 100 delay 50ms rate 20Mbit.*NETEM_CHANGED\nqdisc netem .*limit 100 delay 5ms rate 20Mbit.*NETEM_CLEANED`, string(res.Stdout))
}
