//go:build integration

package migrate

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type deploymentProcess struct {
	command *exec.Cmd
	done    chan struct{}
	logPath string
	err     error
}

func startDeploymentProcess(t *testing.T, binary string, args ...string) *deploymentProcess {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "process.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	command := exec.Command(binary, args...)
	command.Stdout, command.Stderr = logFile, logFile
	require.NoError(t, command.Start())
	p := &deploymentProcess{command: command, done: make(chan struct{}), logPath: logPath}
	go func() {
		p.err = command.Wait()
		_ = logFile.Close()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.stop(t)
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			if len(data) > 12000 {
				data = data[len(data)-12000:]
			}
			t.Logf("%s %v:\n%s", filepath.Base(binary), args, data)
		}
	})
	return p
}

func (p *deploymentProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	// Committer services have no coordinated shutdown API. Stop all writers
	// before taking the database and sidecar-ledger backup.
	require.NoError(t, p.command.Process.Kill())
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("process did not stop: %s", p.command.Path)
	}
}

func reserveDeploymentAddress(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	release := func() { _ = listener.Close() }
	t.Cleanup(release)
	return listener.Addr().String(), release
}

func waitDeploymentAddress(t *testing.T, address string, process *deploymentProcess) {
	t.Helper()
	require.Eventually(t, func() bool {
		select {
		case <-process.done:
			t.Fatalf("%s exited during startup: %v", process.command.Path, process.err)
		default:
		}
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, time.Minute, 100*time.Millisecond, "start %s", address)
}

func runDeploymentCommand(t *testing.T, command *exec.Cmd) []byte {
	t.Helper()
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return output
}

func editDeploymentYAML(t *testing.T, path string, change func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, yaml.Unmarshal(data, &value))
	change(value)
	data, err = yaml.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

type armaDeployment struct {
	root      string
	blockPath string
	start     func()
}

func prepareArmaDeployment(t *testing.T, repository string) armaDeployment {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(repository, "artifacts", "runtime", "bin")
	var parties []map[string]any
	var releases []func()
	for party := 1; party <= 4; party++ {
		entry := map[string]any{"ID": party}
		for _, field := range []string{"RouterEndpoint", "AssemblerEndpoint", "ConsenterEndpoint", "BatchersEndpoints"} {
			address, release := reserveDeploymentAddress(t)
			releases = append(releases, release)
			if field == "BatchersEndpoints" {
				entry[field] = []string{address}
			} else {
				entry[field] = address
			}
		}
		parties = append(parties, entry)
	}
	data, err := yaml.Marshal(map[string]any{"Parties": parties, "MaxPartyID": 4, "UseTLSRouter": "none", "UseTLSAssembler": "none"})
	require.NoError(t, err)
	network := filepath.Join(root, "network.yaml")
	require.NoError(t, os.WriteFile(network, data, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	runDeploymentCommand(t, exec.CommandContext(ctx, filepath.Join(bin, "armageddon"), "generate", "--config", network,
		"--output", root, "--sampleConfigPath", filepath.Join(repository, "artifacts", "runtime", "orderer-sampleconfig")))
	blockPath := filepath.Join(root, "bootstrap", "bootstrap.block")
	type node struct{ role, path, address string }
	var nodes []node
	for party, entry := range parties {
		for _, role := range []struct{ command, file, endpoint string }{
			{"consensus", "consenter", "ConsenterEndpoint"},
			{"batcher", "batcher1", "BatchersEndpoints"},
			{"assembler", "assembler", "AssemblerEndpoint"},
			{"router", "router", "RouterEndpoint"},
		} {
			path := filepath.Join(root, "config", fmt.Sprintf("party%d", party+1), "local_config_"+role.file+".yaml")
			storage := filepath.Join(root, "storage", fmt.Sprintf("party%d", party+1), role.file)
			require.NoError(t, os.MkdirAll(storage, 0o700))
			editDeploymentYAML(t, path, func(config map[string]any) {
				config["FileStore"] = map[string]any{"Location": storage}
				if consensus, ok := config["Consensus"].(map[string]any); ok {
					consensus["WALDir"] = filepath.Join(storage, "wal")
				}
				config["General"].(map[string]any)["LogSpec"] = "warn"
			})
			address, ok := entry[role.endpoint].(string)
			if !ok {
				address = entry[role.endpoint].([]string)[0]
			}
			nodes = append(nodes, node{role.command, path, address})
		}
	}
	return armaDeployment{root: root, blockPath: blockPath, start: func() {
		for _, release := range releases {
			release()
		}
		var processes []*deploymentProcess
		for _, node := range nodes {
			processes = append(processes, startDeploymentProcess(t, filepath.Join(bin, "arma"), node.role, "--config", node.path))
		}
		for i, node := range nodes {
			waitDeploymentAddress(t, node.address, processes[i])
		}
	}}
}

func deploymentDatabaseCommand(t *testing.T, container string, yugabyte bool, user, password, database, operation string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	host := "127.0.0.1"
	tool := "pg_dump"
	if operation == "restore" {
		tool = "psql"
	}
	if yugabyte {
		host = strings.TrimSpace(string(runDeploymentCommand(t, exec.CommandContext(ctx, "docker", "exec", container, "hostname", "-i"))))
		tool = "postgres/bin/ysql_dump"
		if operation == "restore" {
			tool = "bin/ysqlsh"
		}
	}
	args := []string{"exec", "-i", "-e", "PGPASSWORD", container, tool, "-h", host, "-U", user, "-d", database}
	if operation == "restore" {
		args = append(args, "-v", "ON_ERROR_STOP=1")
	} else {
		args = append(args, "--no-owner", "--no-privileges")
	}
	command := exec.CommandContext(ctx, "docker", args...)
	command.Env = append(os.Environ(), "PGPASSWORD="+password)
	return command
}
