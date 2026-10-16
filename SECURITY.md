# Security policy

## Reporting a vulnerability

Please report security problems privately rather than in a public issue.
Once the project is hosted on GitHub, use **Security → Report a
vulnerability** (a private advisory) in the repository. Until then, contact
the maintainer directly.

Please include the SpaceHound version (`spacehound --version`), your macOS
version, and steps to reproduce. Expect an acknowledgement within a week.

## What counts as a vulnerability

SpaceHound moves and removes files, so we treat any way to break the
guarantees in [SAFETY.md](SAFETY.md) as a security bug. That includes:

- Data loss without a typed confirmation, or a permanent unlink anywhere
  other than the verified-offload `remove` step.
- Acting on a file that changed after the scan: a time-of-check/time-of-use
  gap, or a symlink or hard-link swap.
- Overwriting an existing file, or writing outside the chosen volume's
  `SpaceHound Archive` directory (path traversal).
- Removing an original without a verified copy.
- Writing file contents to the operation log, or anything else that leaks
  them.
- `install.sh` installing a binary that hasn't been checksum-verified.

## Design notes

- Go standard library plus a small cgo layer calling macOS system libraries;
  no third-party Go modules. Source builds require Apple's compiler tools.
- No network access, no telemetry, and no privileged operations. Normal use
  never needs `sudo`.
- File changes use held directory/file descriptors, native metadata copying,
  and exclusive moves. Unsupported filesystem guarantees refuse the action.
- Offload removal requires complete fresh content/metadata verification,
  destination synchronization, a durable recovery intent, and runtime-tested
  guarded unlink on the source filesystem. Close writing applications first;
  a write that finishes between final verification and deletion can still
  escape detection. See [SAFETY.md](SAFETY.md) for the limits and recovery flow.
- Terminal output escapes untrusted control characters. Recovery logs reject
  symlinks and unsafe ownership/hard links, enforce private permissions and
  remove extended ACL grants before file-changing operations.
