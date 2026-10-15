package(default_visibility = ["//enterprise:__subpackages__"])

genrule(
    name = "mkinitrd",
    srcs = [
        "//enterprise/server/cmd/goinit",
    ],
    outs = ["initrd.cpio"],
    cmd_bash = """
        env \
            GOINIT=$(execpath //enterprise/server/cmd/goinit:goinit) \
            CPIO=$(execpath //enterprise/tools/cpio) \
            $(execpath mkinitrd.sh) \
            "$@"
    """,
    tools = [
        "mkinitrd.sh",
        "//enterprise/tools/cpio",
    ],
)

genrule(
    name = "vmlinux_copy",
    srcs = select({
        "@platforms//cpu:arm64": ["@org_kernel_git_linux_kernel-vmlinux-arm64//file:downloaded"],
        "//conditions:default": ["//enterprise/vmsupport/kernel:guest_kernel_x86_64_v5_15"],
    }),
    outs = ["vmlinux"],
    cmd = "cp $(SRCS) $@",
)

genrule(
    name = "vmlinux_6.1_copy",
    srcs = ["//enterprise/vmsupport/kernel:guest_kernel_x86_64_v6_1"],
    outs = ["vmlinux_6.1"],
    cmd = "cp $(SRCS) $@",
    target_compatible_with = ["@platforms//cpu:x86_64"],
)
