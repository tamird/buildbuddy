package kernel_test

import (
	"embed"
	"strings"
	"testing"
)

//go:embed microvm-kernel-*.config
var configs embed.FS

func TestGuestKernelConfigs(t *testing.T) {
	requiredOptions := []string{
		"CONFIG_BPF_SYSCALL=y",
		"CONFIG_BPF_JIT=y",
		"CONFIG_BPF_UNPRIV_DEFAULT_OFF=y",
		"CONFIG_CGROUP_BPF=y",
		"CONFIG_DEBUG_FS=y",
		"CONFIG_KPROBES=y",
		"CONFIG_KPROBE_EVENTS=y",
		"CONFIG_UPROBES=y",
		"CONFIG_UPROBE_EVENTS=y",
		"CONFIG_BPF_EVENTS=y",
		"CONFIG_TRACING=y",
		"CONFIG_NETFILTER_XT_MATCH_BPF=y",
		"CONFIG_NET_CLS_BPF=y",
		"CONFIG_NET_ACT_BPF=y",
		"# CONFIG_MODULES is not set",
	}

	x86Options := []string{
		"CONFIG_NFT_FIB_IPV6=y",
		"CONFIG_NFT_FIB_INET=y",
		"CONFIG_NETFILTER_XT_MARK=y",
		"CONFIG_NETFILTER_XT_TARGET_CT=y",
	}
	for _, test := range []struct {
		name              string
		additionalOptions []string
	}{
		{name: "microvm-kernel-aarch64-v5.10.config"},
		{name: "microvm-kernel-x86_64-v5.15.config", additionalOptions: x86Options},
		{name: "microvm-kernel-x86_64-v6.1.config", additionalOptions: x86Options},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, err := configs.ReadFile(test.name)
			if err != nil {
				t.Fatal(err)
			}
			lines := map[string]struct{}{}
			for line := range strings.SplitSeq(string(b), "\n") {
				lines[line] = struct{}{}
			}
			for _, opt := range requiredOptions {
				if _, ok := lines[opt]; !ok {
					t.Errorf("missing %s", opt)
				}
			}
			for _, opt := range test.additionalOptions {
				if _, ok := lines[opt]; !ok {
					t.Errorf("missing %s", opt)
				}
			}
		})
	}
}
