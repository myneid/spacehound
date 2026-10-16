package scan

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SkipRule describes a directory SpaceHound does not descend into by default.
// Exactly one of HomeRel, Abs, Base, Prefix or Suffix is set.
type SkipRule struct {
	HomeRel string // path relative to the home directory
	Abs     string // absolute path
	Base    string // directory name matched anywhere
	Prefix  string // path-name prefix matched anywhere
	Suffix  string // directory name suffix matched anywhere (package bundles)
	Reason  string
	// Overridable rules can be lifted with --include PATH; the others protect
	// Trash, backups and the operating system and are always enforced.
	Overridable bool
}

// DefaultSkipRules are the built-in exclusions. They are documented in
// README.md and printed by `spacehound --show-defaults`.
var DefaultSkipRules = []SkipRule{
	// Failed/interrupted operations retain these originals or working copies.
	{Prefix: ".spacehound-hold-", Reason: "SpaceHound recovery data (restore manually using the operation log)"},
	{Prefix: ".spacehound-probe-", Reason: "SpaceHound filesystem safety probe"},
	{Prefix: ".spacehound-partial-", Reason: "SpaceHound unfinished archive copy (review the operation log)"},
	// Trash: already-deleted items; empty it from Finder.
	{Base: ".Trash", Reason: "Trash (empty it from Finder instead)"},
	{Base: ".Trashes", Reason: "volume Trash"},
	// Time Machine and other backup structures.
	{Base: "Backups.backupdb", Reason: "Time Machine backup"},
	{Base: ".MobileBackups", Reason: "Time Machine local backup"},
	{Base: ".timemachine", Reason: "Time Machine mount"},
	{Suffix: ".backupbundle", Reason: "Time Machine backup bundle"},
	{Suffix: ".sparsebundle", Reason: "disk image bundle internals", Overridable: true},
	{HomeRel: "Library/Application Support/MobileSync/Backup", Reason: "iPhone/iPad backups (manage in Finder > device)", Overridable: true},
	// Cloud storage: files may be dataless placeholders managed by a provider.
	{HomeRel: "Library/Mobile Documents", Reason: "iCloud Drive internals (cloud placeholders)", Overridable: true},
	{HomeRel: "Library/CloudStorage", Reason: "File Provider cloud storage (Dropbox, OneDrive, Google Drive...)", Overridable: true},
	{HomeRel: "Library/Application Support/CloudDocs", Reason: "iCloud sync database", Overridable: true},
	{HomeRel: "Library/Application Support/FileProvider", Reason: "File Provider database", Overridable: true},
	// App-managed libraries: removing files inside corrupts the library.
	{Suffix: ".photoslibrary", Reason: "Photos library (manage inside Photos)", Overridable: true},
	// Filesystem metadata.
	{Base: ".Spotlight-V100", Reason: "Spotlight index"},
	{Base: ".fseventsd", Reason: "filesystem event log"},
	{Base: ".DocumentRevisions-V100", Reason: "document version store"},
	{Base: ".TemporaryItems", Reason: "system temporary items"},
	// Operating system areas (relevant only when --root is outside home).
	{Abs: "/System", Reason: "macOS system volume"},
	{Abs: "/bin", Reason: "operating system"},
	{Abs: "/sbin", Reason: "operating system"},
	{Abs: "/usr", Reason: "operating system"},
	{Abs: "/dev", Reason: "device files"},
	{Abs: "/cores", Reason: "system core dumps (need admin)"},
	{Abs: "/private/var/vm", Reason: "swap and sleep image"},
	{Abs: "/private/var/db", Reason: "system databases"},
	{Abs: "/private/var/folders", Reason: "per-user system temp/caches (managed by macOS)"},
	{Abs: "/Library/Apple", Reason: "Apple system support files"},
	{Abs: "/Volumes", Reason: "other volumes (use --root on the volume instead)", Overridable: true},
	{Abs: "/Network", Reason: "network mounts"},
}

// String renders the rule for --show-defaults.
func (r SkipRule) String() string {
	var what string
	switch {
	case r.HomeRel != "":
		what = "~/" + r.HomeRel
	case r.Abs != "":
		what = r.Abs
	case r.Base != "":
		what = "any directory named " + r.Base
	case r.Prefix != "":
		what = "any path name beginning with " + r.Prefix
	default:
		what = "any directory ending in " + r.Suffix
	}
	o := "always"
	if r.Overridable {
		o = "--include to scan"
	}
	return fmt.Sprintf("%-55s %-48s [%s]", what, r.Reason, o)
}

func (r SkipRule) matches(path, base, home string) bool {
	switch {
	case r.HomeRel != "":
		return strings.EqualFold(path, filepath.Join(home, r.HomeRel))
	case r.Abs != "":
		return strings.EqualFold(path, r.Abs)
	case r.Base != "":
		return strings.EqualFold(base, r.Base)
	case r.Prefix != "":
		return strings.HasPrefix(strings.ToLower(base), strings.ToLower(r.Prefix))
	case r.Suffix != "":
		return strings.HasSuffix(strings.ToLower(base), r.Suffix)
	}
	return false
}

// ProtectedReason reports mandatory exclusions for path or any ancestor.
// It also protects direct roots inside excluded directories, regardless of
// --include or filesystem case sensitivity. Destructive operations share it.
func ProtectedReason(path, home string) string {
	for cur := filepath.Clean(path); ; cur = filepath.Dir(cur) {
		for _, r := range DefaultSkipRules {
			if !r.Overridable && r.matches(cur, filepath.Base(cur), home) {
				return r.Reason
			}
		}
		if parent := filepath.Dir(cur); parent == cur {
			return ""
		}
	}
}

// within reports whether path equals dir or is inside it.
func within(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// skipReason returns why dir should not be scanned, or "".
func (o *Options) skipReason(path string, isDir bool) string {
	if reason := ProtectedReason(path, o.Home); reason != "" {
		return reason
	}
	base := filepath.Base(path)
	for _, ex := range o.Excludes {
		if strings.HasPrefix(ex, "/") {
			if within(path, ex) {
				return "excluded by --exclude " + ex
			}
		} else if ok, _ := filepath.Match(ex, base); ok {
			return "excluded by --exclude " + ex
		}
	}
	if !isDir {
		return ""
	}
	for _, r := range DefaultSkipRules {
		if !r.matches(path, base, o.Home) {
			continue
		}
		if r.Overridable && (within(o.Root, path) || o.included(path)) {
			continue
		}
		return r.Reason
	}
	return ""
}

func (o *Options) included(path string) bool {
	for _, inc := range o.Includes {
		if within(path, inc) {
			return true
		}
	}
	return false
}

// homeRel returns path relative to home using forward slashes, or "" if the
// path is not inside home.
func homeRel(path, home string) string {
	if !within(path, home) || path == home {
		return ""
	}
	return filepath.ToSlash(path[len(home)+1:])
}

func hasPrefixFold(rel string, prefixes ...string) (string, bool) {
	lr := strings.ToLower(rel)
	for _, p := range prefixes {
		lp := strings.ToLower(p)
		if lr == lp || strings.HasPrefix(lr, lp+"/") {
			return p, true
		}
	}
	return "", false
}

var (
	appDataRoots = []string{
		"Library/Application Support",
		"Library/Containers",
		"Library/Group Containers",
		"Library/Mail",
		"Library/Messages",
		"Library/Safari",
		"Library/Keychains",
		"Library/Accounts",
		"Library/Calendars",
		"Library/Photos",
	}
	devRuntimeRoots = []string{
		"Library/Developer/CoreSimulator",
		"Library/Developer/Xcode/iOS DeviceSupport",
		"Library/Developer/Xcode/watchOS DeviceSupport",
		"Library/Developer/Xcode/Archives",
		"Library/Developer/Toolchains",
		"Library/Android/sdk",
		".android/avd",
		".rustup",
		".cargo",
		".pyenv",
		".rbenv",
		".nvm",
		".sdkman",
		".gradle/wrapper",
		".docker",
		".colima",
		".orbstack",
		".lima",
		"go/pkg/mod",
		".m2/repository",
		"miniconda3",
		"anaconda3",
		".conda",
	}
	dbExts = map[string]bool{
		".db": true, ".sqlite": true, ".sqlite3": true, ".sqlite-wal": true, ".sqlite-shm": true,
		".db-wal": true, ".db-shm": true, ".realm": true, ".ldb": true, ".mdb": true, ".ibd": true,
		".frm": true, ".wal": true, ".kdbx": true, ".storedata": true, ".fdb": true,
	}
	vmExts = map[string]bool{
		".vmdk": true, ".vdi": true, ".qcow2": true, ".vhd": true, ".vhdx": true, ".hdd": true,
		".hds": true, ".raw": true, ".img": true, ".vmem": true, ".vmsn": true, ".nvram": true,
	}
	vmBundleSuffixes = []string{".pvm/", ".utm/", ".vmwarevm/", ".vbox/", ".parallels/"}
	chainWords       = []string{"bitcoin", "ethereum", "chaindata", "geth", "monero", "bitmonero", "lightning", "solana", "litecoin", "blockchain"}
)

// sensitivity explains why a path is application data, a database, a VM
// image, blockchain data or a developer runtime. Such files are never treated
// as disposable cache, and large files there are marked dangerous.
func sensitivity(path, rel string) string {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	ext := filepath.Ext(base)
	switch {
	case dbExts[ext]:
		return "looks like a database (" + ext + "); deleting can corrupt an app's data"
	case isLevelDBLog(base):
		return "looks like a LevelDB/IndexedDB write-ahead log, part of a live database"
	case vmExts[ext]:
		return "looks like a virtual machine or disk image (" + ext + ")"
	case base == "docker.raw" || strings.Contains(lower, "/com.docker.docker/"):
		return "Docker virtual disk; reclaim space with Docker's own prune tools"
	}
	for _, s := range vmBundleSuffixes {
		if strings.Contains(lower, s) {
			return "inside a virtual machine bundle (" + strings.TrimSuffix(s, "/") + ")"
		}
	}
	scope := strings.ToLower(rel)
	if rel == "" {
		scope = lower
	}
	for _, comp := range strings.Split(scope, "/") {
		comp = strings.TrimPrefix(comp, ".")
		for _, w := range chainWords {
			if comp == w || strings.HasPrefix(comp, w+"-") || strings.HasSuffix(comp, "-"+w) {
				return "looks like blockchain/wallet data (" + comp + ")"
			}
		}
	}
	if p, ok := pathRoot(path, devRuntimeRoots); ok {
		return "developer runtime or toolchain (" + p + "); use the tool's own cleanup command"
	}
	if p, ok := pathRoot(path, appDataRoots); ok {
		return "application data (" + p + "); apps may need this file"
	}
	return ""
}

// pathRoot matches complete component sequences anywhere in an absolute
// path. Application data and runtimes remain sensitive outside our home,
// including other users' homes, system Library and mounted archives.
func pathRoot(path string, roots []string) (string, bool) {
	lower := "/" + strings.Trim(strings.ToLower(filepath.ToSlash(filepath.Clean(path))), "/") + "/"
	for _, root := range roots {
		if strings.Contains(lower, "/"+strings.ToLower(root)+"/") {
			return root, true
		}
	}
	return "", false
}

// isLevelDBLog matches LevelDB write-ahead logs such as "000123.log".
func isLevelDBLog(base string) bool {
	name, ok := strings.CutSuffix(base, ".log")
	if !ok || len(name) < 6 {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
