// A local run of the CI live job's checks.
//
// THE SHELL SCRIPT IN THE WORKFLOW CANNOT RUN EVERYWHERE. It uses wget, which a slim development
// machine does not have, and the failures it looks for — a status code parsed wrongly, a request that
// silently fell through to the console — are exactly the ones worth catching in seconds rather than in
// a five-minute CI round trip.
//
// This program does the same things with the standard library, so it runs anywhere Go does:
//
//	go run ./internal/host/livecheck
//
// IT IS NOT A REPLACEMENT FOR THE WORKFLOW. The workflow exercises the same paths through the same
// binaries a deployment runs; this is the fast loop. When one changes, change both.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	failures int
	podPort  = "18099"
	upAddr   = "127.0.0.1:18098"
	base     = "http://127.0.0.1:" + podPort
)

func failf(format string, args ...any) {
	failures++
	fmt.Printf("  FAIL: "+format+"\n", args...)
}

func okf(format string, args ...any) { fmt.Printf("  ok   "+format+"\n", args...) }

// get performs a request and returns the status and body.
func get(method, url string, body string, hdr map[string]string) (int, string) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		return 0, err.Error()
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func main() {
	work, err := os.MkdirTemp("", "livecheck-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	dataDir := filepath.Join(work, "data")
	srvDir := filepath.Join(work, "srv")
	upDir := filepath.Join(work, "upstream")
	for _, d := range []string{dataDir, filepath.Join(srvDir, "assets"), upDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			panic(err)
		}
	}
	if err := os.WriteFile(filepath.Join(srvDir, "index.html"), []byte("<!doctype html><html>CONSOLE</html>"), 0o644); err != nil {
		panic(err)
	}

	fmt.Println("--- building ---")
	podBin := filepath.Join(work, "pod")
	build := exec.Command("go", "build", "-o", podBin, "./cmd/ext-ui-host")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("build failed:", err)
		os.Exit(1)
	}

	// THE STAND-IN UPSTREAM answers every path with JSON. A miss that falls through to the console is
	// then a CONTENT question — is the body that JSON or is it index.html — rather than a status-code
	// one, because index.html is also a 200.
	upSrc := filepath.Join(upDir, "main.go")
	_ = os.WriteFile(upSrc, []byte(upstreamSource), 0o644)
	upBin := filepath.Join(upDir, "server")
	init := exec.Command("go", "mod", "init", "upstream")
	init.Dir = upDir
	_ = init.Run()
	b2 := exec.Command("go", "build", "-o", upBin, ".")
	b2.Dir = upDir
	if err := b2.Run(); err != nil {
		fmt.Println("upstream build failed:", err)
		os.Exit(1)
	}

	fmt.Println("--- starting ---")
	up := exec.Command(upBin, upAddr)
	up.Stdout, up.Stderr = os.Stdout, os.Stderr
	if err := up.Start(); err != nil {
		panic(err)
	}
	defer func() { _ = up.Process.Kill() }()

	pod := exec.Command(podBin)
	pod.Env = append(os.Environ(),
		"UI_DATA_DIR="+dataDir,
		"CONSOLE_DIR="+srvDir,
		"PORT="+podPort,
		"ADMIN_TOKEN=ci-token",
		"AGENT_UPSTREAM="+upAddr,
		"EXT_SVC_SUFFIX=.invalid",
	)
	pod.Stdout, pod.Stderr = os.Stdout, os.Stderr
	if err := pod.Start(); err != nil {
		panic(err)
	}
	defer func() { _ = pod.Process.Kill() }()

	// WAITING ON THE PORT, not on a sleep: the pod binds after it has read its console directory, and a
	// fixed sleep is either too short on a slow machine or wasted on a fast one.
	if !waitFor("127.0.0.1:"+podPort, 15*time.Second) {
		fmt.Println("the pod never started listening")
		os.Exit(1)
	}

	fmt.Println("--- the console ---")
	if st, body := get("GET", base+"/", "", nil); st != 200 || !strings.Contains(body, "CONSOLE") {
		failf("/ = %d, body %q", st, trunc(body))
	} else {
		okf("/ serves the console")
	}
	if st, body := get("GET", base+"/some/deep/route", "", nil); st != 200 || !strings.Contains(body, "CONSOLE") {
		failf("the SPA fallback = %d, body %q", st, trunc(body))
	} else {
		okf("/some/deep/route falls back to the console")
	}
	if st, _ := get("GET", base+"/assets/missing.js", "", nil); st != 404 {
		failf("a missing console asset = %d, want 404 (answering it with HTML breaks the page silently)", st)
	} else {
		okf("a missing console asset is a 404")
	}

	fmt.Println("--- the agent, on the same port ---")
	// THE ROUTE THAT REGRESSED ONCE: `/agent.v1.X/Method` cannot be a ServeMux pattern, and when it
	// fell through, the console answered its own RPC with index.html.
	st, body := get("POST", base+"/agent.v1.AgentService/ListSessions", "{}",
		map[string]string{"Content-Type": "application/json", "Connect-Protocol-Version": "1"})
	if !strings.Contains(body, `"from":"upstream"`) {
		failf("the agent RPC did not reach the upstream (%d): %q", st, trunc(body))
	} else {
		okf("the agent RPC reaches its upstream")
	}
	if !strings.Contains(body, "/agent.v1.AgentService/ListSessions") {
		failf("the proxy rewrote the path: %q", trunc(body))
	} else {
		okf("the path is passed through unchanged")
	}

	fmt.Println("--- an extension's API leaves this process ---")
	_, body = get("GET", base+"/ext/gitdev/api/v1/whoami", "", nil)
	if strings.Contains(body, "CONSOLE") {
		failf("an extension API call fell through to the console")
	} else {
		okf("an extension API call is not the console")
	}

	fmt.Println("--- nothing installed ---")
	if st, _ := get("GET", base+"/ext/gitdev/api/v1/ui", "", nil); st != 404 {
		failf("discovery with nothing installed = %d, want 404", st)
	} else {
		okf("discovery says 404, so the console shows no card")
	}
	if st, _ := get("GET", base+"/ext/gitdev/", "", nil); st != 404 {
		failf("an uninstalled panel = %d, want 404", st)
	} else {
		okf("an uninstalled panel says 404")
	}

	fmt.Println("--- install, then activate ---")
	auth := map[string]string{
		"Content-Type": "application/json", "Connect-Protocol-Version": "1",
		"Authorization": "Bearer ci-token",
	}
	st, body = get("POST", base+"/admin/ui.v1.UiHostService/Install",
		`{"extId":"gitdev","repo":"easydev-org/easyagent-gitdev-ext-ui","version":"latest"}`, auth)
	if st != 200 {
		failf("install = %d: %q", st, trunc(body))
		fmt.Printf("\n%d failure(s)\n", failures)
		os.Exit(1)
	}
	var inst struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal([]byte(body), &inst)
	okf("installed %s", inst.Version)

	// INSTALLING MUST NOT SERVE — that separation is what makes an install safe to attempt.
	if st, _ := get("GET", base+"/ext/gitdev/api/v1/ui", "", nil); st != 404 {
		failf("an install activated the panel (%d)", st)
	} else {
		okf("an install does not activate")
	}

	st, body = get("POST", base+"/admin/ui.v1.UiHostService/Activate",
		fmt.Sprintf(`{"extId":"gitdev","version":%q}`, inst.Version), auth)
	if st != 200 {
		failf("activate = %d: %q", st, trunc(body))
	}

	fmt.Println("--- the panel, on the console's own origin ---")
	st, body = get("GET", base+"/ext/gitdev/api/v1/ui", "", nil)
	if st != 200 || !strings.Contains(body, `"base"`) {
		failf("discovery = %d: %q", st, trunc(body))
	} else if !strings.Contains(body, `"descriptions"`) {
		failf("discovery has no descriptions — ui.json did not reach the bundle: %q", trunc(body))
	} else {
		okf("discovery answers with the panel's own metadata")
	}
	if st, body := get("GET", base+"/ext/gitdev/", "", nil); st != 200 || !strings.Contains(strings.ToLower(body), "<html") {
		failf("the panel bundle = %d: %q", st, trunc(body))
	} else {
		okf("the panel bundle serves")
	}
	if st, _ := get("GET", base+"/ext/gitdev/assets/nope.js", "", nil); st != 404 {
		failf("a missing panel asset = %d, want 404", st)
	} else {
		okf("a missing panel asset is a 404")
	}

	fmt.Println("--- the serving version cannot be removed ---")
	if st, body := get("POST", base+"/admin/ui.v1.UiHostService/Remove",
		fmt.Sprintf(`{"extId":"gitdev","version":%q}`, inst.Version), auth); st != 400 {
		failf("removing the serving version = %d, want 400: %q", st, trunc(body))
	} else {
		okf("removing the serving version is refused")
	}

	fmt.Println("--- the console still works with a panel installed ---")
	if st, body := get("GET", base+"/", "", nil); st != 200 || !strings.Contains(body, "CONSOLE") {
		failf("the console stopped serving once a panel was active (%d)", st)
	} else {
		okf("the console and the panel share the port")
	}

	if failures > 0 {
		fmt.Printf("\n%d failure(s)\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nall live checks passed")
}

func trunc(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

const upstreamSource = `package main

import (
	"net/http"
	"os"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(` + "`" + `{"from":"upstream","path":"` + "`" + ` + r.URL.Path + ` + "`" + `"}` + "`" + `))
	})
	_ = http.ListenAndServe(os.Args[1], mux)
}
`
