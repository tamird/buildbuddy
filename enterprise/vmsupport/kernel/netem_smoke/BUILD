load("@rules_python//python:defs.bzl", "py_test")

exports_files(["retained-vmlinux"])

py_test(
    name = "netem_smoke_test",
    srcs = ["netem_smoke_test.py"],
    args = [
        "$(location @netem_smoke_tools//:flat)",
        "$(location :retained-vmlinux)",
        "$(location :init.sh)",
    ],
    data = [
        "init.sh",
        "retained-vmlinux",
        "@netem_smoke_tools//:flat",
    ],
    exec_properties = {
        "test.EstimatedComputeUnits": "2",
        "test.EstimatedMemory": "2GB",
    },
    tags = ["manual"],
    target_compatible_with = [
        "@platforms//cpu:x86_64",
        "@platforms//os:linux",
    ],
)
