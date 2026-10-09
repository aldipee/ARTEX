package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// These tests cover two safeguards:
//  1. Delivery destinations must not use the server to reach private networks or cloud metadata (SSRF).
//  2. URL validation errors must not expose credentials from the URL.
//
// Many package tests use httptest receivers on 127.0.0.1, which the guard blocks
// by default. TestMain enables AllowLocalTargetsEnv, while each SSRF case clears
// it explicitly to verify the default-deny behavior.

func TestMain(m *testing.M) {
	// Allow regular tests to reach local fake receivers; SSRF cases clear this temporarily.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core SSRF assertion: by default,
// a loopback destination must be rejected at the connection layer.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // Disable the opt-in to test the default behavior.
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("loopback delivery should be disabled by default")
	}
	if hit {
		t.Fatal("request reached a local service; guard did not take effect")
	}
	// The error should tell users how to opt in; a local SMTP relay is a valid setup.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection should explain how to opt in: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the inverse case: loopback must work
// when explicitly enabled so legitimate local Postfix or relay setups remain usable.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work after explicitly opting in: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // Cloud metadata endpoint; the main reason this guard exists.
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped form must be unmapped before checking, or it becomes a bypass.
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private addresses are intentionally allowed because self-hosted
	// Mattermost and SMTP relays are common. This assertion makes the tradeoff explicit.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private networks are common valid destinations)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets verifies that literal IPs are
// rejected during config validation instead of failing on the first delivery.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected during config validation", raw)
		}
	}
	// Public and private hostnames remain valid; private-network policy is enforced when dialing.
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials covers the url.Parse failure path.
// A parse error is a *url.Error containing the full original URL. Invalid schemes
// such as file://, gopher://, and ftp:// parse successfully and do not exercise it.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // Invalid percent escape.
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // Non-numeric port.
		"http://[::1?access_token=" + leakProbeToken,                  // Unmatched bracket.
	}
	for _, raw := range cases {
		// First verify that this input actually fails url.Parse; otherwise the test
		// could silently exercise a different branch.
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should fail to parse or this case does not cover the intended branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Verify that channel-level error wrapping does not expose the URL either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("invalid URL should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the SMTP dial guard. The email
// channel previously used a bare net.Dialer, leaving a gap that allowed connections
// to 169.254.169.254 or 127.0.0.1. SMTP handshake errors could also echo peer data
// through delivery history. TestMain enables local targets for other tests, so this
// case must clear the setting to prove the default guard is active.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // Disable the opt-in to test the default behavior.
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("email delivery to loopback should be disabled by default")
	}
	// The guard should reject in the Control hook before a connection or EHLO is sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP session was established; guard did not take effect")
	}
	// The error should tell users how to opt in; a local Postfix relay is a valid setup.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection should explain how to opt in: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired inverse case: delivery
// must work when explicitly enabled for common private SMTP and local relay setups.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP delivery should work after opting in: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO was not observed; the SMTP session was not established")
	}
}
