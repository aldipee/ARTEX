package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("masked value leaked the full credential: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("masked value should not expose the destination host: %q", got)
	}
	// Retain the last six characters so users can identify the bot.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("expected the last six characters to remain as a hint: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("IsMasked should recognize the masked value: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// Revealing the last six characters of a short credential would expose it entirely.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("a credential of length %d should not show a tail hint; got %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("masked value contains the original value: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s should be masked; got %q", k, s)
		}
	}
	// Non-secret fields must remain unchanged so the UI can display them.
	if masked["port"] != float64(587) {
		t.Errorf("non-secret field port should not change: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// For an unknown channel kind, show an empty config instead of returning raw content that may contain credentials.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("unknown channel kind should return an empty config; got %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// Masking is a display-layer operation and must not alter stored values.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig mutated its input and could overwrite real credentials with masked values")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// The user changed only the method, so the browser submits masked values plus the new method.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("masked fields should retain their stored values; got %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("updated field should take effect; got %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("an empty string should clear the field; got %v", got)
	}
	// Unmentioned fields are preserved (partial-update semantics).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("unmentioned field should be preserved; got %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("unmentioned fields should be preserved; got %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("mentioned field should be updated; got %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap protects a core security invariant:
// changing the destination must not carry old credentials to the new endpoint.
//
// These cases use attacker-shaped inputs that change only the destination and
// omit credentials; testing only valid inputs would not prove the defense works.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing is the credential key expected in the error.
		wantMissing string
	}{
		{
			name: "webhook destination changed while reusing Authorization header",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram base_url changed while reusing the Bot Token",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "email SMTP host changed while reusing the password",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "email TLS changed and password must be explicitly supplied",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// A masked value means "reuse the old credential" and must also be rejected after a destination change.
			name:        "masked credential returned with a new destination",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk webhook changed while reusing the signing secret",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("destination change without renewed credentials should be rejected; got config %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("expected a dedicated error type so the API can provide actionable guidance; got %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected missing credential key %q to be named; got %v", tc.wantMissing, target.Missing)
			}
			// The error should tell the operator how to fix the problem.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("error should mention %q: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits ensures valid edits are not blocked,
// which could otherwise encourage users to bypass or remove the safeguard.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "rename only (config returned unchanged)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "change request method only; destination and credentials stay the same",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "change destination and provide new credentials",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "change destination and explicitly remove credentials",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "change Telegram chat_id (not the destination)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "change email recipients (not the destination)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("valid edit was incorrectly blocked: %v", err)
			}
			if merged == nil {
				t.Fatal("expected a merged config")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance covers a subtle comparison case:
// JSON ports may be float64 while submitted values may be int. Comparing directly
// would treat an unchanged port as a destination change and show a false password warning.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// Submit the same port as an int.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("equal port values with different types should not count as a destination change: %v", err)
	}
	// A genuinely different port must be blocked.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("port change should be blocked")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination covers a blank
// destination field: an empty Telegram base_url means use the official API.
//
// Previously, the channel could no longer be saved after the second save:
//
//	base_url:"" is stored on create because that path saves the submitted config directly
//	→ first save treats the empty string as an explicit clear and deletes the key
//	→ second save still submits "", but stored no longer has the key, so it looks like a destination change
//	→ bot_token is masked → 400 "destination changed; re-enter credentials"
//
// The user had changed nothing, but saving failed until the Bot Token was pasted again.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// Frontend buildConfig() submits every configured field: credentials use their
	// masked values and blank text fields use empty strings. Reproduce that payload.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// First save: only the channel name changed, so config is returned unchanged.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("first save was incorrectly blocked: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("precondition changed: MergeConfig should remove the empty string; this case covers the subsequent missing-key state")
	}

	// Second save: the submitted payload is unchanged.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("second save was incorrectly blocked even though nothing changed: %v", err)
	}
	// A third save confirms that the behavior remains stable.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("third save was incorrectly blocked: %v", err)
	}
	// The credential must be retained throughout and not cleared with the blank field.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token should retain its original value; got %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges pairs with the prior
// case: treating blank and missing keys as equivalent must not allow real destination
// changes. Telegram's Bot Token is in the URL path, so changing base_url exposes it.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// Direction one: change from blank (official endpoint) to a custom endpoint.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("changing from the official endpoint to a custom one must require a new Token")
	}

	// Direction two: clearing a custom endpoint (returning to the official API) is also a destination change.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("clearing a custom endpoint must also require a new Token")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// As with SecretKeys, PrepareConfigUpdate cannot protect a channel that omits its destination keys.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("channel %s has no destination keys; credential protection on destination changes is ineffective", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s has no credential keys", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// The compiler requires every channel to implement SecretKeys. Also verify that
	// no channel returns an empty slice, which would expose credentials in the browser.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("channel %s has no masking expectation in this test", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s declares no credential fields, so its config would be returned in plain text", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer covers a masking edge case:
// MergeConfig recognizes masked values only as prefixed strings, so a sentinel
// nested inside an object such as webhook.headers could otherwise be stored literally.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// Embed a masking sentinel inside an object.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("a masking sentinel nested in an object should be rejected")
	}
	// Accept a fully submitted object containing a real new value.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("a valid new header should not be blocked: %v", err)
	}
}
