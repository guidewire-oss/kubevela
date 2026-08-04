/*
Copyright 2026 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers_test

import (
	"fmt"
	"net/http"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

// mockAddonServerPort is e2e/addon/mock's hardcoded listen port
// (e2e/addon/mock/utils.Port); it is not configurable, so callers must free it
// before starting a new instance.
const mockAddonServerPort = 9098

// mockAddonServerProcess is a handle to a running `go run ./e2e/addon/mock`
// subprocess.
type mockAddonServerProcess struct {
	cmd *exec.Cmd
}

// startMockAddonServerProcess frees mockAddonServerPort (killing whatever the
// shared e2e setup left listening there, if anything -- safe by the time this
// suite runs, since the e2e/addon and test/e2e-addon-test suites that might
// still be using it have already finished earlier in the pipeline) and starts
// the real e2e/addon/mock binary via `go run` from repoRoot, waiting until it
// answers HTTP requests.
//
// Its main() unconditionally calls utils.ApplyMockServerConfig(), which
// overwrites the shared "KubeVela"/"Test-Helm" addon registry ConfigMap
// entries and creates a "test-vela" namespace copy. Callers are responsible
// for snapshotting and restoring any registry state they care about.
func startMockAddonServerProcess(repoRoot string) (*mockAddonServerProcess, error) {
	// #nosec G204 -- fixed command, not user input
	killCmd := exec.Command("bash", "-c", fmt.Sprintf("kill -9 $(lsof -ti:%d) 2>/dev/null || true", mockAddonServerPort))
	if err := killCmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to free port %d before starting e2e/addon/mock: %w", mockAddonServerPort, err)
	}

	cmd := exec.Command("go", "run", "./e2e/addon/mock") // #nosec G204 -- fixed command, not user input
	cmd.Dir = repoRoot
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start e2e/addon/mock: %w", err)
	}

	proc := &mockAddonServerProcess{cmd: cmd}
	url := fmt.Sprintf("http://127.0.0.1:%d/", mockAddonServerPort)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(url) // #nosec G107 -- fixed local URL, not user input
		if err == nil {
			_ = resp.Body.Close()
			return proc, nil
		}
		if time.Now().After(deadline) {
			_ = proc.Stop()
			return nil, fmt.Errorf("e2e/addon/mock did not become ready on port %d: %w", mockAddonServerPort, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Stop kills the mock server subprocess.
func (p *mockAddonServerProcess) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return nil
}
