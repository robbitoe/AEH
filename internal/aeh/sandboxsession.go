package aeh

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Agent sessions (stage 6, section 6). HOME is disposable per launch; only the files a
// profile declares in sandbox.session persist, in a store private to the runner that is
// never mounted. Before a launch they are copied into a fresh HOME volume; after the
// process exits, each one read back complete replaces the stored copy.
const (
	maxSessionFiles  = 16
	sessionFileLimit = int64(1) << 20
	sessionLoginTime = 30 * time.Minute
)

var (
	sessionPathPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// Names Windows reserves for devices, with or without an extension.
	windowsDeviceName = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\..*)?$`)
)

// checkSessionPaths validates a profile's sandbox.session. The paths are relative to
// HOME and become paths of the host store too, so each part is a plain portable name.
func checkSessionPaths(paths []string) error {
	if len(paths) > maxSessionFiles {
		return fmt.Errorf("at most %d files", maxSessionFiles)
	}
	seen := map[string]string{}
	for _, p := range paths {
		if err := checkTreePath(p); err != nil {
			return err
		}
		for _, part := range strings.Split(p, "/") {
			if !sessionPathPart.MatchString(part) || strings.HasSuffix(part, ".") || windowsDeviceName.MatchString(part) {
				return fmt.Errorf("invalid path %q: use letters, digits, '.', '_' and '-'", p)
			}
		}
		key := strings.ToLower(p)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("%q repeats %q", p, prev)
		}
		seen[key] = p
	}
	for _, p := range paths {
		for _, q := range paths {
			if strings.HasPrefix(strings.ToLower(q), strings.ToLower(p)+"/") {
				return fmt.Errorf("%q is inside %q, which is a file", q, p)
			}
		}
	}
	return nil
}

// sessionDir is a profile's store. The name keeps the profile readable and a digest
// keeps two profiles apart whatever their names.
func (s *sandbox) sessionDir(profile string) string {
	sum := sha256.Sum256([]byte(profile))
	return filepath.Join(s.dir, "sessions", flowKeyUnsafe.ReplaceAllString(profile, "_")+"-"+hex.EncodeToString(sum[:6]))
}

// lockSession serializes everything that reads or writes a profile's store: launches
// from copy-in to write-back, and logins. The lock is the operating system's, so a
// process that dies releases it. It waits until ctx ends.
func lockSession(ctx context.Context, dir string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("session lock: %w", err)
	}
	f, err := os.OpenFile(dir+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session lock: %w", err)
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		ok, err := tryLockFile(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("session lock: %w", err)
		}
		if ok {
			return func() { f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("waiting for the session of another launch of this profile: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// loadSession reads the stored files of a profile. A file never saved is skipped.
func loadSession(dir string, paths []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, p := range paths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		fi, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("session file %s: %w", p, err)
		}
		if !fi.Mode().IsRegular() || fi.Size() > sessionFileLimit {
			return nil, fmt.Errorf("session file %s: not a regular file of at most %d bytes", p, sessionFileLimit)
		}
		b, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("session file %s: %w", p, err)
		}
		files[p] = b
	}
	return files, nil
}

// storeSessionFile replaces one stored file atomically, mode 0600.
func storeSessionFile(dir, p string, data []byte) error {
	full := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

// errSessionMissing marks a declared file the container's HOME does not hold.
var errSessionMissing = errors.New("not present in HOME")

// readSessionFile reads one declared file from a stopped container's HOME: exactly one
// regular, non-empty file of at most sessionFileLimit bytes, read to the end.
func (s *sandbox) readSessionFile(ctx context.Context, cid, p string) ([]byte, error) {
	var data []byte
	err := s.readBack(ctx, cid, sandboxHome+"/"+p, func(r io.Reader) error {
		tr := tar.NewReader(&cappedReader{r: r, left: sessionFileLimit + 64<<10})
		h, err := tr.Next()
		if err == io.EOF {
			// docker cp writes nothing for a path that does not exist; an existing one
			// always yields an entry.
			return errSessionMissing
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || h.Name != path.Base(p) {
			return errors.New("not a regular file")
		}
		if h.Size <= 0 || h.Size > sessionFileLimit {
			return fmt.Errorf("size %d outside 1..%d bytes", h.Size, sessionFileLimit)
		}
		if data, err = io.ReadAll(tr); err != nil {
			return err
		}
		if int64(len(data)) != h.Size {
			return errors.New("incomplete")
		}
		if _, err := tr.Next(); err != io.EOF {
			return errors.New("more than one entry")
		}
		return nil
	})
	if errors.Is(err, errSessionMissing) {
		// Docker failing before any output reads the same; either way the stored copy
		// is kept, and the report names a file that was stored.
		return nil, errSessionMissing
	}
	return data, err
}

// sessionReport is the evidence of a write-back: the files saved and, for the others,
// why the stored copy was kept.
type sessionReport struct {
	Saved []string `json:"saved,omitempty"`
	Kept  []string `json:"kept,omitempty"`
}

// saveSession reads each declared file back and replaces the stored copy only when it
// was read complete and non-empty; any problem keeps the previous copy. A file that was
// never stored and is still absent is not a problem.
func (s *sandbox) saveSession(ctx context.Context, cid, dir string, paths []string, before map[string][]byte) sessionReport {
	var rep sessionReport
	for _, p := range paths {
		data, err := s.readSessionFile(ctx, cid, p)
		if errors.Is(err, errSessionMissing) && before[p] == nil {
			continue
		}
		if err == nil && bytes.Equal(data, before[p]) {
			continue
		}
		if err == nil {
			err = storeSessionFile(dir, p, data)
		}
		if err != nil {
			rep.Kept = append(rep.Kept, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		rep.Saved = append(rep.Saved, p)
	}
	return rep
}

// prepareSession locks a profile's store and returns its HOME volume and the write-back
// for its launch. A profile without a session gets nothing. A profile with one but no
// stored file cannot run: the owner logs in first.
func (s *sandbox) prepareSession(ctx context.Context, name string, p Profile) (vol *sandboxVolume, save func(cid string) sessionReport, unlock func(), err error) {
	paths := p.sandboxSession()
	if len(paths) == 0 {
		return nil, nil, func() {}, nil
	}
	dir := s.sessionDir(name)
	unlock, err = lockSession(ctx, dir)
	if err != nil {
		return nil, nil, nil, err
	}
	files, err := loadSession(dir, paths)
	if err != nil {
		unlock()
		return nil, nil, nil, err
	}
	if len(files) == 0 {
		unlock()
		return nil, nil, nil, fmt.Errorf("profile %q has no saved session; run: aeh sandbox login %s", name, name)
	}
	vol = &sandboxVolume{Target: sandboxHome, Private: true, Fill: treeFiles(files, nil)}
	save = func(cid string) sessionReport { return s.saveSession(ctx, cid, dir, paths, files) }
	return vol, save, unlock, nil
}

// SandboxLogin runs a profile's login inside the agent image, attached to the terminal,
// with a fresh HOME volume, and saves the declared session files it leaves there. The
// login never sees the project: no /work, no /memory.
func SandboxLogin(ctx context.Context, r Repo, name string, in *os.File, out, errw io.Writer) error {
	cfg, err := r.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.Sandbox == nil || cfg.Sandbox.AgentImage == "" {
		return errors.New("aeh sandbox login needs sandbox.agent_image in aeh/config.yaml: agents run on the host and use their own logins")
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	paths := p.sandboxSession()
	if len(paths) == 0 {
		return fmt.Errorf("profile %q declares no sandbox.session files to keep", name)
	}
	argv := p.Sandbox.Login
	if len(argv) == 0 {
		if len(p.Command) == 0 {
			return fmt.Errorf("profile %q has no command", name)
		}
		argv = []string{p.Command[0], "login"}
	}
	env, err := agentEnv(p, p.sandboxPassEnv())
	if err != nil {
		return fmt.Errorf("profile environment: %w", err)
	}
	sb, err := openSandbox(r, *cfg.Sandbox, "login")
	if err != nil {
		return err
	}
	dir := sb.sessionDir(name)
	unlock, err := lockSession(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	tty := false
	if fi, err := in.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	var rep sessionReport
	res, err := sb.run(ctx, sandboxLaunch{Kind: "login", Argv: argv, Env: env, Workdir: sandboxHome, Pids: true,
		Timeout: sessionLoginTime, Agent: true, Interactive: &interactiveIO{In: in, Out: out, Err: errw, TTY: tty},
		Volumes: []sandboxVolume{{Target: sandboxHome, Private: true, Fill: treeFiles(nil, nil)}},
		SaveSession: func(cid string, res LaunchResult) {
			if res.ExitCode == 0 {
				rep = sb.saveSession(ctx, cid, dir, paths, nil)
			}
		}}, nil)
	if err != nil {
		return err
	}
	if res.Class != ClassProcess || res.ExitCode != 0 {
		return fmt.Errorf("the login did not finish (%s, exit %d %s); no session was saved", res.Class, res.ExitCode, res.Reason)
	}
	for _, k := range rep.Kept {
		fmt.Fprintf(errw, "warning: session file %s\n", k)
	}
	if len(rep.Saved) == 0 {
		return fmt.Errorf("the login left none of the declared files (%s) in HOME; check sandbox.session of profile %q", strings.Join(paths, ", "), name)
	}
	sort.Strings(rep.Saved)
	fmt.Fprintf(out, "saved the session of profile %s: %s\n", name, strings.Join(rep.Saved, ", "))
	return nil
}
