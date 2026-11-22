# AUR packaging

Two packages for Arch Linux and Arch-based distributions (including Omarchy):

| Package | Kind | Contents |
| --- | --- | --- |
| [`noema`](noema/PKGBUILD) | source | Builds from the signed GitHub release tarball with `cargo build --release --locked`. Needs `rust` and `cmake` at build time. |
| [`noema-bin`](noema-bin/PKGBUILD) | prebuilt | Installs the official `noema_<ver>_linux_{amd64,arm64}.tar.gz` release binary. |

Both install `/usr/bin/noema`, the MIT license, `README.md` under
`/usr/share/doc/noema/`, and shell completions for bash, zsh, and fish.
`noema-bin` declares `provides=('noema')` / `conflicts=('noema')`.

## Release update checklist

When a new version `X.Y.Z` is published (after the release workflow has
uploaded assets):

1. Source archive hash:
   `curl -sL https://github.com/Fail-Safe/Noema/archive/refs/tags/vX.Y.Z.tar.gz | sha256sum`
   → update `pkgver` and `sha256sums` in `noema/PKGBUILD`.
2. Binary hashes: read them from the release's `checksums.txt`
   (`noema_X.Y.Z_linux_amd64.tar.gz`, `noema_X.Y.Z_linux_arm64.tar.gz`)
   → update `pkgver` and `sha256sums_{x86_64,aarch64}` in `noema-bin/PKGBUILD`.
3. Regenerate `.SRCINFO` in each package directory: `makepkg --printsrcinfo > .SRCINFO`.
4. Verify a clean build (macOS host, Docker; `--platform linux/amd64` and
   `--security-opt seccomp=unconfined` are needed because the official image is
   amd64-only and pacman's sandbox needs syscalls Docker's default profile blocks):
   `docker run --rm --platform linux/amd64 --security-opt seccomp=unconfined -v "$PWD:/build" -w /build archlinux bash -c 'pacman -Syu --noconfirm base-devel rust cmake && useradd -m build && chown -R build /build && runuser -u build -- bash -c "cd /build/noema && makepkg -sc --noconfirm"'`
5. Push both directories to their AUR repos (`ssh://aur@aur.archlinux.org/noema.git`,
   `.../noema-bin.git`).

The `archlinux` Docker image sets `NoExtract = usr/share/doc/*` in its
`pacman.conf`, so `/usr/share/doc/noema/README.md` is absent after installing
the test package there even though `pacman -Ql` lists it. That is the image's
global config, not a packaging defect; real systems extract it.

## Notes

- The source build fetches crates with `cargo fetch --locked` in `prepare()`;
  `cmake` is required because `rusqlite` bundles SQLite and `ring`/`aws-lc-sys`
  build native code.
- `check()` runs only the two storage-critical integration suites
  (`nosync_layout`, `database_maintenance`), matching the release workflow;
  the full suite is run by CI before the tag exists.
- Musl variants exist upstream as release assets but are not packaged; the
  glibc archives are the supported Arch path.
