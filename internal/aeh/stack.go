package aeh

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// topicStack marks the discovery question about the technology stack.
const topicStack = "stack"

// projectStack is what the main checkout's committed files say about its technology stack.
type projectStack struct {
	Greenfield bool   // no committed file besides aeh's own and repository housekeeping
	Evidence   string // manifests, file extensions and configured commands, one per line
}

// stackManifests are files whose presence names a stack.
var stackManifests = map[string]bool{
	"package.json": true, "deno.json": true, "go.mod": true, "pyproject.toml": true, "requirements.txt": true,
	"setup.py": true, "Pipfile": true, "Cargo.toml": true, "pom.xml": true, "build.gradle": true,
	"build.gradle.kts": true, "Gemfile": true, "composer.json": true, "mix.exs": true, "pubspec.yaml": true,
	"Package.swift": true, "CMakeLists.txt": true, "Makefile": true,
}

// housekeeping reports a committed file that does not make a project "existing": aeh's own
// files and top-level repository housekeeping (ignore and attribute files, readme, license,
// changelog and other top-level Markdown).
func housekeeping(p string) bool {
	if strings.HasPrefix(p, "aeh/") {
		return true
	}
	if strings.Contains(p, "/") {
		return false
	}
	lower := strings.ToLower(p)
	switch lower {
	case ".gitignore", ".gitattributes", ".gitmodules", ".editorconfig":
		return true
	}
	// Exact names only, with a text extension at most: license.py is code, not a license.
	stem, ext := lower, path.Ext(lower)
	if ext == ".md" || ext == ".txt" || ext == ".rst" {
		stem = strings.TrimSuffix(lower, ext)
	} else if ext != "" {
		return false
	}
	switch stem {
	case "readme", "license", "licence", "copying", "changelog", "notice", "authors", "contributing":
		return true
	}
	return ext == ".md"
}

// detectStack reads the committed files at the main checkout's HEAD. A repository without
// commits starts from scratch.
func detectStack(r Repo) (projectStack, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return projectStack{}, err
	}
	if _, err := git(main, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return projectStack{Greenfield: true}, nil
	}
	raw, err := gitRaw(main, "ls-tree", "-r", "--name-only", "-z", "HEAD")
	if err != nil {
		return projectStack{}, fmt.Errorf("detect the project's stack: %w", err)
	}
	var manifests []string
	exts := map[string]int{}
	code := 0
	for _, p := range strings.Split(raw, "\x00") {
		if p == "" || housekeeping(p) {
			continue
		}
		code++
		base := path.Base(p)
		if stackManifests[base] || strings.HasSuffix(base, ".csproj") || strings.HasSuffix(base, ".fsproj") || strings.HasSuffix(base, ".sln") {
			manifests = append(manifests, p)
		}
		if ext := path.Ext(base); ext != "" {
			exts[ext]++
		}
	}
	if code == 0 {
		return projectStack{Greenfield: true}, nil
	}
	var lines []string
	sort.Strings(manifests)
	for i, m := range manifests {
		if i == 20 {
			lines = append(lines, fmt.Sprintf("... and %d more manifests", len(manifests)-20))
			break
		}
		lines = append(lines, "manifest: "+m)
	}
	type count struct {
		ext string
		n   int
	}
	var counts []count
	for e, n := range exts {
		counts = append(counts, count{e, n})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].n != counts[j].n {
			return counts[i].n > counts[j].n
		}
		return counts[i].ext < counts[j].ext
	})
	for i, c := range counts {
		if i == 5 {
			break
		}
		lines = append(lines, fmt.Sprintf("files: %d *%s", c.n, c.ext))
	}
	if cfg, err := (Repo{Root: main}).LoadConfig(); err == nil {
		if cfg.Commands.Build != "" {
			lines = append(lines, "build command: "+cfg.Commands.Build)
		}
		lines = append(lines, "test command: "+cfg.Commands.Test)
	}
	// Repository paths and the owner's commands reach the prompt as fenced reference data.
	return projectStack{Evidence: redactDiagnostic(strings.Join(lines, "\n"))}, nil
}
