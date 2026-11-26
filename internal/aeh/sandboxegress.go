package aeh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/robbitoe/aeh/internal/egress"
)

// Container paths and addresses of an agent's network (stage 6, S3).
const (
	egressDir    = "/egress" // the socket volume: the proxy's only link to the launch
	egressSocket = egressDir + "/proxy.sock"
	egressPolicy = "/run/aeh-egress" // the proxy's read-only policy
	egressListen = "127.0.0.1:3128"  // the forwarder inside the agent's container
	egressURL    = "http://" + egressListen
	// egressNoProxy keeps loopback traffic off the proxy; the container has nothing else.
	egressNoProxy = "localhost,127.0.0.1,::1"
	// Proxy containers are small and get their own limits.
	egressMemory = "256m"
	egressCPUs   = "1"
	// egressLogLimit bounds the proxy's log copied into the run directory.
	egressLogLimit = int64(16) << 20
)

// proxyValue is a proxy variable's value: empty, or the launch's forwarder.
func proxyValue(name string, proxied bool) string {
	if !proxied {
		return ""
	}
	switch name {
	case "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy":
		return egressURL
	case "NO_PROXY", "no_proxy":
		return egressNoProxy
	}
	return "" // FTP_PROXY and ALL_PROXY stay empty
}

// startEgress creates and starts the proxy of launch id: a container from the agent image
// with Docker's default network, the policy at a read-only mount and a socket volume it
// shares with the launch and nothing else. Every object carries the launch's labels, so
// release and recovery remove them with the launch. It returns the socket volume and the
// proxy's container.
func (s *sandbox) startEgress(ctx context.Context, id, kind string, pairs []string) (socketVolume, container string, err error) {
	pol := egress.DefaultPolicy(append([]string(nil), pairs...))
	if err := pol.Validate(); err != nil {
		return "", "", err
	}
	data, err := json.MarshalIndent(pol, "", "  ")
	if err != nil {
		return "", "", err
	}
	// Indexes above the launch's own volumes keep the names apart.
	vols := []sandboxVolume{
		{Target: egressDir, Fill: treeFiles(nil, nil)},
		{Target: egressPolicy, ReadOnly: true, Fill: treeFiles(map[string][]byte{"policy.json": data}, nil)},
	}
	for i := range vols {
		if vols[i].Name, err = s.createVolume(id, kind, 100+i); err != nil {
			return "", "", err
		}
	}
	if err := s.fill(ctx, id, kind, s.agent.ID, vols); err != nil {
		return "", "", err
	}
	l := sandboxLaunch{Kind: kind, Argv: []string{"aeh-egress", "proxy", "--socket", egressSocket, "--policy", egressPolicy + "/policy.json"},
		Workdir: egressDir, Agent: true, Network: "bridge"}
	own, want, err := s.launchEnv(s.agent, nil, false)
	if err != nil {
		return "", "", err
	}
	envFile, err := s.writeEnvFile(id+"-egress", own)
	if err != nil {
		return "", "", err
	}
	defer os.Remove(envFile)
	args := append([]string{"create", "--pull=never"}, s.labels(id, kind, "egress")...)
	args = append(args, "--network", "bridge", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", egressMemory, "--memory-swap", egressMemory, "--cpus", egressCPUs, "--read-only", "--pids-limit", "128",
		"--tmpfs", "/tmp:mode=1777", "--tmpfs", sandboxHome+":uid=10001,gid=10001,mode=0700",
		"--user", sandboxUser, "--workdir", l.Workdir, "--env-file", envFile, "--entrypoint", l.Argv[0],
		"--log-driver", "json-file", "--log-opt", "max-size="+sandboxLogKeep)
	for _, v := range vols {
		m := "type=volume,src=" + v.Name + ",dst=" + v.Target + ",volume-nocopy"
		if v.ReadOnly {
			m += ",readonly"
		}
		args = append(args, "--mount", m)
	}
	args = append(append(args, s.agent.ID), l.Argv[1:]...)
	cid, err := s.d.admin(args...)
	if err != nil {
		return "", "", err
	}
	created, err := s.inspect(cid)
	if err != nil {
		return "", "", err
	}
	if err := s.checkCreated(created, l, vols, want); err != nil {
		return "", "", err
	}
	if _, err := s.d.run(ctx, nil, nil, "start", cid); err != nil {
		return "", "", err
	}
	return vols[0].Name, cid, nil
}

// endEgress checks that the proxy is still running after the launch, stops it and copies
// its log to w (when not nil). A proxy that stopped on its own is an error.
func (s *sandbox) endEgress(ctx context.Context, cid string, w io.Writer) error {
	st, err := s.inspect(cid)
	if err != nil {
		return err
	}
	running := st.State.Status == "running"
	// A stop sends SIGTERM first: the proxy closes its tunnels and logs each one's bytes.
	if _, err := s.d.admin("stop", "--time", "10", cid); err != nil && running {
		return err
	}
	if w != nil {
		if err := s.logs(ctx, cid, w, nil); err != nil {
			return fmt.Errorf("read the proxy log: %w", err)
		}
	}
	if !running {
		return fmt.Errorf("the proxy stopped during the launch (state %s, exit %d%s)", st.State.Status, st.State.ExitCode, stateError(st))
	}
	return nil
}

// agentNetworkName is what an agent launch's evidence records as its network.
func agentNetworkName(pairs []string) string {
	if len(pairs) > 0 {
		return "egress"
	}
	return "none"
}
