# Third-party source notices

This repository is a source-complete integration tree for local builds of HiDeck.
It keeps the visible project-level source dependencies in `third_party/` so the
build no longer depends on the unavailable `github.com/iniwex5/vowifi-go`
repository or on unavailable upstream release binaries.

## Included source trees

| Path | Upstream | Commit checked | License |
| --- | --- | --- | --- |
| `.` | `https://github.com/giszh86/vohive` | `f240894e763cf7f0cf74c88562bb9b55f0d573b1` | PolyForm Noncommercial 1.0.0 |
| `third_party/netlink` | `https://github.com/iniwex5/netlink` | module cache `v1.3.3` | MIT |
| `third_party/qqbot` | `https://github.com/iniwex5/qqbot` | module cache `v1.0.1` | MIT |
| `third_party/quectel-qmi-go` | `https://github.com/iniwex5/quectel-qmi-go` | `aaada14395c19ee4c8b4b15a373f41bd2ed14cf0` (`v0.6.0`) | MIT |
| `third_party/vowifi-go` | `https://github.com/boa-z/vowifi-go` | `23459976796fb43b024c479b19b6edf8baf379d4` | AGPL-3.0 |

The PC/SC reader backend dynamically loads `github.com/ebitengine/purego`
(`v0.10.2`, Apache-2.0) to call the platform WinSCard/PCSC-Lite API without
requiring CGO.

The original `github.com/iniwex5/vowifi-go v1.1.2` module was not publicly
accessible at the time this integration tree was assembled. The replacement
under `third_party/vowifi-go` is an independent open implementation that uses
the same module path for compatibility.

## Build notes

The root `go.mod` uses local `replace` directives:

```go
replace (
	github.com/iniwex5/netlink => ./third_party/netlink
	github.com/iniwex5/qqbot => ./third_party/qqbot
	github.com/iniwex5/quectel-qmi-go => ./third_party/quectel-qmi-go
	github.com/iniwex5/vowifi-go => ./third_party/vowifi-go
)
```

In-app binary self-updates are disabled for this integration tree. Release
binaries and Docker images should be updated through repository releases or
container image rollout.

UPX compression is disabled by default in `Dockerfile.github` to keep produced
binaries easier to inspect.

## Optional QDC507 voice runtime

The experimental `internal/modemvoice/qdc507` adapter can verify a local runtime
bundle or explicitly download it into a versioned cache. These runtime binaries
are not embedded in the Go executable or copied into the Docker images.

- Upstream: https://github.com/moluncn/mavo
- Pinned revision: `0443dfdaf8aec086fd76ba2ee9152fd908114524`
- Runtime directory: `Resources/ModuleVoice`
- Runtime version: `qdc507-3.18.44-voice-20260712.5`
- Verified files include the upstream `COPYING-GPL-2.0` and `MODULE-REPORT.md`.
- `mavo-pcm-bridge.armv7` is supplied by the MIT-licensed MaVo project; the kernel
  objects `qdc507_aprv3.ko` and `qdc507_voice.ko` advertise GPL v2 module metadata.

The repository records fixed sizes and SHA-256 hashes for the selected artifacts.
The report bundled upstream describes an older runtime revision in places; the
pinned artifact list, not that prose, determines which modules are loaded.
Any future redistribution of these binary artifacts must separately address
their corresponding source and applicable notices. This experimental downloader
does not establish that a public combined runtime distribution is ready.

## License compatibility note

The root project license and the included AGPL-3.0 VoWiFi implementation carry
different obligations. This file documents the source origins; it does not grant
additional rights or resolve license compatibility for public combined binary or
container distribution.
