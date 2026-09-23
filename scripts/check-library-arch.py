#!/usr/bin/env python3
"""Fail unless a shared library is the container format and CPU the release claims.

release.sh already checks the archive name and the file inside it; this closes the
remaining hole: a cross target can exit 0 and still produce the wrong image, for
example a darwin/amd64 row shipping an arm64 Mach-O because the C compiler ignored
GOARCH. The check reads only the header, so it works on every host.

    scripts/check-library-arch.py <goos> <goarch> <library>
"""
import struct
import sys

# (container format, machine field) per Go target, taken from the format specs:
# ELF e_machine, Mach-O cputype and COFF machine.
TARGETS = {
    ("linux", "amd64"): ("ELF", 0x3E),
    ("linux", "arm64"): ("ELF", 0xB7),
    ("linux", "arm"): ("ELF", 0x28),
    ("darwin", "amd64"): ("Mach-O", 0x01000007),
    ("darwin", "arm64"): ("Mach-O", 0x0100000C),
    ("windows", "amd64"): ("PE", 0x8664),
    ("windows", "arm64"): ("PE", 0xAA64),
}


def identify(head):
    """Return (format, machine) for an ELF, 64-bit Mach-O or PE header, else None."""
    if head[:4] == b"\x7fELF" and len(head) >= 20:
        return "ELF", struct.unpack_from("<H", head, 18)[0]
    # cf fa ed fe is MH_MAGIC_64 in little-endian byte order.
    if head[:4] == b"\xcf\xfa\xed\xfe" and len(head) >= 8:
        return "Mach-O", struct.unpack_from("<I", head, 4)[0]
    if head[:2] == b"MZ" and len(head) >= 0x40:
        offset = struct.unpack_from("<I", head, 0x3C)[0]
        if len(head) >= offset + 6 and head[offset:offset + 4] == b"PE\0\0":
            return "PE", struct.unpack_from("<H", head, offset + 4)[0]
    return None


def main(argv):
    if len(argv) != 4:
        return f"usage: {argv[0]} <goos> <goarch> <library>"
    goos, goarch, path = argv[1], argv[2], argv[3]
    target = (goos, goarch)
    if target not in TARGETS:
        return f"no architecture check for {goos}/{goarch}"
    format_name, machine = TARGETS[target]

    with open(path, "rb") as handle:
        found = identify(handle.read(0x1000))
    if found is None:
        return f"{path} is not an ELF, Mach-O or PE image"
    if found != (format_name, machine):
        return (
            f"{path} is {found[0]} machine 0x{found[1]:x}, "
            f"want {format_name} machine 0x{machine:x} for {goos}/{goarch}"
        )
    print(f"{path}: {format_name} machine 0x{machine:x} ({goos}/{goarch})")
    return None


if __name__ == "__main__":
    problem = main(sys.argv)
    if problem is not None:
        sys.exit(f"release: {problem}")
