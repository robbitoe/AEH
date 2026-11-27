package aeh

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// newInspectionExport is the read-only view of an adoption inspector under the sandbox:
// the commit's regular files without what newInspectionWorktree strips on the host.
// Links and submodules are left out (an export holds no links), and so is every file
// that possibleAdoptSecret matches, as the host walk removes them. The excluded paths
// are returned for the prompt and the evidence.
func newInspectionExport(root, commit string, exclude []string) (*commitExport, []string, error) {
	out, err := git(root, "ls-tree", "-r", "-z", "--long", "--full-tree", commit)
	if err != nil {
		return nil, nil, fmt.Errorf("export %s: %w", commit, err)
	}
	e := &commitExport{root: root, Commit: commit}
	excluded := map[string]bool{}
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, nil, fmt.Errorf("export %s: unexpected ls-tree entry %q", commit, entry)
		}
		if err := checkTreePath(name); err != nil {
			return nil, nil, fmt.Errorf("the sandbox cannot export %s: %w", short(commit), err)
		}
		mode, kind, blob := fields[0], fields[1], fields[2]
		if kind != "blob" || (mode != "100644" && mode != "100755") || possibleAdoptSecret(name, exclude) {
			excluded[name] = true // links, submodules and possible secrets
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("export %s: size of %s: %w", commit, name, err)
		}
		e.Files = append(e.Files, exportFile{Path: name, Blob: blob, Executable: mode == "100755", Size: size})
	}
	list := make([]string, 0, len(excluded))
	for p := range excluded {
		if err := checkDisplayLine(p, 500); err != nil {
			return nil, nil, err
		}
		if err := checkNoSecrets("excluded path", []byte(p)); err != nil {
			return nil, nil, err
		}
		list = append(list, p)
	}
	sort.Strings(list)
	sort.Slice(e.Files, func(i, j int) bool { return e.Files[i].Path < e.Files[j].Path })
	return e, list, nil
}
