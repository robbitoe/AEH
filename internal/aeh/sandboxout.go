package aeh

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// agentOutput is what an agent left in its /out view, read back from the stopped
// container into memory: nothing it wrote touches the host's file system.
type agentOutput struct {
	Files map[string][]byte
	Dirs  []string
}

// readAgentOutput parses the tar of /out. Only directories and regular files are
// accepted, at most maxFiles files and maxTotal bytes; anything else is the agent's
// failure (agentContentError), named by its path.
func readAgentOutput(r io.Reader, maxFiles int, maxTotal int64) (agentOutput, error) {
	out := agentOutput{Files: map[string][]byte{}}
	// The stream limit leaves room for tar headers and padding beyond the content.
	tr := tar.NewReader(&cappedReader{r: r, left: maxTotal + int64(maxFiles+64)*2048})
	var total int64
	tooLarge := func() error { return agentContent("/out holds more than %d files or %d bytes", maxFiles, maxTotal) }
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return out, nil
		case errors.Is(err, errReadBackLimit):
			return agentOutput{}, tooLarge()
		case err != nil:
			return agentOutput{}, fmt.Errorf("read back /out: %w", err)
		}
		name := strings.TrimSuffix(path.Clean(strings.TrimPrefix(h.Name, "./")), "/")
		if name == "out" {
			if h.Typeflag != tar.TypeDir {
				return agentOutput{}, agentContent("/out is not a directory")
			}
			continue
		}
		rel, ok := strings.CutPrefix(name, "out/")
		if !ok || checkTreePath(rel) != nil {
			return agentOutput{}, agentContent("/out: unexpected entry %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Dirs = append(out.Dirs, rel)
			continue
		case tar.TypeReg:
		default:
			return agentOutput{}, agentContent("/out/%s is not a regular file or directory; write plain files only", rel)
		}
		if len(out.Files) >= maxFiles || h.Size < 0 || total+h.Size > maxTotal {
			return agentOutput{}, tooLarge()
		}
		total += h.Size
		b, err := io.ReadAll(io.LimitReader(tr, h.Size))
		switch {
		case errors.Is(err, errReadBackLimit):
			return agentOutput{}, tooLarge()
		case err != nil:
			return agentOutput{}, fmt.Errorf("read back /out/%s: %w", rel, err)
		case int64(len(b)) != h.Size:
			return agentOutput{}, fmt.Errorf("read back /out/%s: short read", rel)
		}
		out.Files[rel] = b
	}
}

// freeze applies the same checks as freezeOutput to an output read back from a
// container: names representable on every host, the manifest's files and limits.
func (o agentOutput) freeze(m outputManifest) (frozenOutput, error) {
	for _, d := range o.Dirs {
		if err := checkOutputName(d); err != nil {
			return frozenOutput{}, fmt.Errorf("%s output: %s: %w", m.Name, d, err)
		}
	}
	if err := m.check(o.Files); err != nil {
		return frozenOutput{}, err
	}
	return frozenOutput{Manifest: m.Name, Digest: outputDigest(m.Name, o.Files), Files: o.Files}, nil
}
