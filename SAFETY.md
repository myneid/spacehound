# SpaceHound safety model

SpaceHound provides read-only inventory, confirmed same-volume Trash moves,
and verified offloads with a separate permanent-removal choice. It refuses
operations when it cannot establish the required filesystem guarantees.
Keep backups and close applications that may write files before removing
their originals; verification cannot make a changing file immutable.

## Scanning and display

- Scanning and `--json` do not create, rename or remove files. Selection
  does not change files either. Duplicate hashing reads regular files only.
- Directory traversal and hashing use held descriptors without following
  symlinks, including ancestor directories. A directory replacement cannot
  redirect a subsequent read through its replacement symlink.
- Mandatory exclusions cover the Trash, recovery and partial-copy names,
  backup structures, filesystem metadata and protected OS areas. They apply
  to the chosen root and its ancestors, regardless of capitalization.
  `--include` overrides only rules explicitly marked overridable.
- Cloud storage and managed libraries are skipped by default. Dataless
  placeholders are not read, even when their containing tree is included.
- Filenames and error text are escaped before terminal rendering, including
  control characters and bidirectional formatting characters. Filesystem
  operations and JSON retain the original path values.

## Classification and confirmation

- Risk levels are `low`, `review` and `dangerous`; overlapping detectors
  choose the most cautious result. Selecting all visible skips `dangerous`.
- Databases, VM/disk images, blockchain data, known application-data paths
  and developer runtimes are never disposable cache. This also applies
  outside the current user's home, including other users' application data.
- A stale log is `low` only with age, size and a recognized user-log root.
  A `.log` suffix by itself is insufficient. Managed caches and downloaded
  ML models require review.
- Mutating UI actions show their files, sizes, likely savings and risk
  counts, then require `trash`, `offload` or `remove` to be typed.
- Actions reopen the source without following symlinks and check its device,
  inode, size, modification time, change time and hard-link count against the
  scan identity. A later rename can change change time, so captured files
  are checked again using identity facts and complete fingerprints.

## Trash moves

SpaceHound moves regular files into an existing same-volume Trash, using
native descriptor-relative exclusive renames. It never substitutes an
overwriting rename when the filesystem lacks exclusive-move support.
Collisions choose a different name. An unusable Trash leaves the original
in place.

Before moving an original, SpaceHound records its intent and captures it in
a private holding directory beside the source. It verifies the captured
identity, moves it to the Trash, and checks the moved identity. Failure
triggers a non-overwriting restoration attempt. If that is blocked, the
file is preserved and its recovery location is reported. If verification
fails after a move into Trash, SpaceHound retains the descriptor-identified
original at its current location instead of attempting rollback through a
shared Trash pathname that another process could have replaced.

Trash moves do not permanently unlink originals or immediately reclaim
space. The operation log records both original and final paths. Restore
manually from the reported Trash location; Finder's “Put Back” support is
not promised.

## Offload and permanent removal

1. Only regular files with one hard link can be offloaded. The destination
   must be a suitable writable, local external volume. Internal, network,
   hidden, backup and read-only volumes are rejected. Free space must include
   headroom; FAT volumes cannot accept files of 4 GiB or more.
2. The destination is
   `<volume>/SpaceHound Archive/<hostname>/<YYYY-MM-DD>/<relative path>`.
   Traversal components and symlink ancestors are rejected. Created archive
   ancestors are synchronized before descending into them. An occupied
   destination is refused before hashing or copying the source.
3. Data is copied through held descriptors to a new partial file. Native
   metadata copying preserves resource forks, extended attributes, ACLs,
   permissions, ownership, modification/creation times and preservable
   flags. There is no metadata-losing fallback.
4. Verification compares SHA-256 of the main contents and a second
   fingerprint covering resource-fork contents and metadata. It checks
   source stability before and after copying/hashing. Access/change times,
   inode numbers, allocation and filesystem-specific compression/tracking
   state are not expected to survive a copy. Unsupported or mismatching
   metadata refuses verification.
5. Only a matching, flushed copy is published using an atomic exclusive
   move. The destination directory and file are synchronized. Existing
   destinations are never overwritten. Failure retains the source and
   reports any partial or published copy for inspection.
6. A separate confirmation offers `remove`, `trash`, or keeping both. Before
   permanent removal, SpaceHound reopens and recalculates the complete
   fingerprints of both files and checks the destination volume identity.
7. A disposable probe on the source filesystem must prove that guarded
   unlink rejects open files, writable mappings without open descriptors,
   and files with multiple hard links. Unsupported flags or filesystems
   refuse permanent removal; Trash and keeping both remain available.
8. The original is captured in a private same-filesystem holding directory
   after a durable journal intent. Its complete fingerprint is verified
   again, the destination is reverified and flushed, and directory/source
   identities are checked. SpaceHound closes its source descriptor, then
   uses guarded unlink on the captured entry.

Close applications that may write these files before choosing `remove`.
Guarded unlink prevents removal while a writer remains open or mapped,
but a write that completes between final verification and deletion can
escape detection. These checks do not provide a transaction across two
filesystems or protect against concurrent changes to the archive afterward.

Verification refuses resource forks of 4 GiB or more, ordinary extended
attributes above 64 MiB, and unexpectedly large attribute lists or ACLs.
Filesystems that cannot preserve or flush required metadata also fail safely.

## Recovery journal

`~/Library/Logs/SpaceHound/operations.log` is a JSON-lines journal. Before
file-changing operations, its directory/file are required to be caller-owned,
without symlinks, with one hard link for the file. Their permissions are
enforced as `0700`/`0600`, and extended ACL grants are removed. The journal
records paths, sizes, hashes, phases and outcomes, never file contents.
Within a running process, an unchanged journal reuses its validated recovery
state. External changes trigger a full replay; detected truncation blocks
further writes. Each append verifies the prior bytes against their validated
checksum before updating cached state. This still reads historical bytes,
while avoiding repeated JSON decoding and detecting overlapping writes that
ignore the journal locks.

An unavailable or unreadable journal blocks file-changing operations. Intent
records are flushed before a source is captured. Failure or cancellation
restores captured originals without replacing another file. If restoration
is blocked, the original is preserved at `recovery_path`, commonly
`<source directory>/.spacehound-hold-<id>/original`.

Interactive startup reports unfinished operations and their recovery paths.
An unfinished record may describe an action that completed before its final
record was written; inspect both original and recovery locations before
restoring manually. SpaceHound does not automatically delete recovery files.
`--json` remains an inventory-only path and does not initialize the journal.

## Free space and demo mode

SpaceHound never deletes APFS/Time Machine snapshots or runs privileged
commands. Savings estimates account for allocated bytes and hard links;
clones, snapshots and open files can reduce actual reclaimed space.

`--demo` creates a fixture under `/private/tmp` with its own home, volumes,
Trash and journal. All actions stay within that fixture, which is removed on exit.
Tests use generated fixtures and cover refused operations as well as success.

## Native API references

Apple explicitly treats a resource fork as content rather than ordinary
metadata: [extendedAttributes](apple-docs://fileprovider/nsfileprovideritemprotocol/extendedattributes).
SpaceHound includes its bytes in complete verification.

The native layer follows the SDK headers and local macOS manuals for
`openat(2)`, `fstatat(2)`, `renameatx_np(2)`, `unlinkat(2)`, `fcopyfile(3)`,
`fsetattrlist(2)`, `getxattr(2)`, `acl_copy_ext(3)` and `fcntl(2)`.
These libc entry points were not found in the Cupertino documentation index;
runtime capability probes are required in addition to compilation.
