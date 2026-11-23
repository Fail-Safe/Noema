# AUR packaging

Two packages for Arch Linux and Arch-based distributions (including Omarchy):

| Package | Kind | Contents |
| --- | --- | --- |
| [`noema`](noema/PKGBUILD) | source | Builds from the signed GitHub release tarball with `cargo build --release --locked`. Needs `rust` and `cmake` at build time. |
| [`noema-bin`](noema-bin/PKGBUILD) | prebuilt | Installs the official `noema_<ver>_linux_{amd64,arm64}.tar.gz` release binary. |

Both install `/usr/bin/noema`, the MIT license, `README.md` under
`/usr/share/doc/noema/`, and shell completions for bash, zsh, and fish.
`noema-bin` declares `provides=('noema')` / `conflicts=('noema')`.

## Publication status

AUR new-account registration is paused (since 2026-06, Arch malicious-packages
incident; see archlinux.org/news and aur-general), so these packages are not
yet published. Until registration reopens, Arch/Omarchy users can install
directly from this repo:

```bash
git clone https://github.com/Fail-Safe/Noema && cd Noema/packaging/aur/noema-bin
makepkg -si   # or the noema/ directory to build from source
```

Both directories are push-ready (`ssh://aur@aur.archlinux.org/noema{,-bin}.git`).

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
4. Verify a clean build (on Linux: `makepkg -sc --noconfirm` directly, or in
   an `archlinux` container). On a macOS Docker host the official image is
   amd64-only and pacman's sandbox fails under the default seccomp profile —
   pass `--disable-sandbox` to every `pacman` call (not `--security-opt
   seccomp=unconfined`, which does not fix it):
   `docker run --rm -v "$PWD:/build" -w /build archlinux bash -c 'pacman -Syu --noconfirm --disable-sandbox base-devel rust cmake && useradd -m build && chown -R build /build && runuser -u build -- bash -c "cd /build/noema && makepkg -sc --noconfirm"'`
5. Push both directories to their AUR repos (`ssh://aur@aur.archlinux.org/noema.git`,
   `.../noema-bin.git`).

## Verification matrix (v0.24.2-1)

| Environment | noema (source) | noema-bin |
| --- | --- | --- |
| Omarchy VM, native aarch64 | built, 14 tests pass, packaged binary runs `v0.24.2` | built (arm64 hash validated), packaged binary runs `v0.24.2` |
| `archlinux` container, native x86_64 | built, packaged binary runs `v0.24.2` | built, `pacman -U` install, `noema version` OK, completions + license present |

The source PKGBUILD sets `options=('!lto')`: Arch `makepkg` enables LTO by
default, injecting `-flto` into `CFLAGS`, which the `cc` crate propagates into
the `aws-lc-sys` and `libsqlite3-sys` static archives. The final rustc link
(both `rust-lld` and GNU `ld`) then fails with undefined `aws_lc_0_44_0_*` and
`sqlite3_*` symbols. `build()` also omits `--target` — AUR builds are native,
and the flag adds nothing; upstream CI builds the glibc artifacts the same way.

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
