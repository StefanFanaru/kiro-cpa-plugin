package main

import (
	"encoding/json"
	"net/url"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// startTestLogin runs StartLogin the way CPA does and returns the response.
func startTestLogin(t *testing.T) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw, err := handleLoginStart([]byte(`{"provider":"kiro","base_url":"http://127.0.0.1:8317/v8/management/oauth/callback"}`))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result json.RawMessage `json:"result"`
	}
	var login pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response.Result, &login); err != nil {
		t.Fatal(err)
	}
	return login
}

// keepLoginSettings restores the globals a configure call writes.
func keepLoginSettings(t *testing.T) {
	t.Helper()
	settings, base := pluginSettings, currentLoginBaseURL()
	t.Cleanup(func() {
		pluginSettings = settings
		loginBaseURL.Store(base)
	})
	pluginSettings = testPluginSettings
	loginBaseURL.Store("")
}

func TestNormalizeLoginBaseURL(t *testing.T) {
	valid := map[string]string{
		"":                           "",
		"http://127.0.0.1:8317":      "http://127.0.0.1:8317",
		"  http://127.0.0.1:8317/  ": "http://127.0.0.1:8317",
		"https://cpa.example.com":    "https://cpa.example.com",
		"HTTPS://cpa.example.com/":   "https://cpa.example.com",
		"http://[::1]:8317":          "http://[::1]:8317",
		"http://tiny-server:8317":    "http://tiny-server:8317",
	}
	for raw, want := range valid {
		got, ok := normalizeLoginBaseURL(raw)
		if !ok || got != want {
			t.Errorf("normalizeLoginBaseURL(%q) = %q, %v; want %q, true", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"127.0.0.1:8317",
		"localhost:8317",
		"//127.0.0.1:8317",
		"ftp://127.0.0.1:8317",
		"javascript:alert(1)",
		"http://",
		"http:127.0.0.1",
		"http://127.0.0.1:8317/v8/management",
		"http://127.0.0.1:8317/v8/management/plugins/kiro-ha/login",
		"http://127.0.0.1:8317?next=1",
		"http://127.0.0.1:8317?",
		"http://127.0.0.1:8317#state=x",
		"http://127.0.0.1:8317#",
		"http://user:pass@127.0.0.1:8317",
		"http://127.0.0.1:port",
		"http://127.0.0.1:8317\n/evil",
	} {
		if got, ok := normalizeLoginBaseURL(raw); ok || got != "" {
			t.Errorf("normalizeLoginBaseURL(%q) = %q, %v; want rejected", raw, got, ok)
		}
	}
}

func TestLoginLinkStaysRelativeWithoutLoginBaseURL(t *testing.T) {
	// The Management Center resolves the link against its own origin; the
	// default must remain exactly the relative link it has always received.
	keepLoginSettings(t)
	configurePlugin(configureRequest("plugins:\n  configs:\n    kiro-ha:\n      min_token_interval: 4s\n"))
	login := startTestLogin(t)
	if want := resourceBasePath + loginResourcePath + "#state=" + login.State; login.URL != want {
		t.Fatalf("sign-in URL = %q, want %q", login.URL, want)
	}
}

func TestLoginBaseURLMakesTheLinkAbsolute(t *testing.T) {
	for name, doc := range map[string]string{
		"plugin section": "login_base_url: http://127.0.0.1:8317/\n",
		"plugin id":      "plugins:\n  configs:\n    kiro-ha:\n      login_base_url: http://127.0.0.1:8317/\n",
		"provider name":  "plugins:\n  configs:\n    kiro:\n      login_base_url: http://127.0.0.1:8317\n",
	} {
		t.Run(name, func(t *testing.T) {
			keepLoginSettings(t)
			configurePlugin(configureRequest(doc))
			if pluginSettings.LoginBaseURL != "http://127.0.0.1:8317" || pluginSettings.MinTokenInterval != "3s" || pluginSettings.SuspendCooldown != "2h" {
				t.Fatalf("settings after configure: %#v", pluginSettings)
			}
			login := startTestLogin(t)
			want := "http://127.0.0.1:8317" + resourceBasePath + loginResourcePath + "#state=" + login.State
			if login.URL != want {
				t.Fatalf("sign-in URL = %q, want %q", login.URL, want)
			}
			// EasyCLIProxyAPI accepts the link only if it parses on its own.
			parsed, err := url.Parse(login.URL)
			if err != nil || !parsed.IsAbs() || parsed.Host != "127.0.0.1:8317" || parsed.Fragment != "state="+login.State {
				t.Fatalf("sign-in URL %q does not parse as an absolute link: %v", login.URL, err)
			}
		})
	}
}

func TestInvalidLoginBaseURLFallsBackToTheRelativeLink(t *testing.T) {
	keepLoginSettings(t)
	configurePlugin(configureRequest("plugins:\n  configs:\n    kiro-ha:\n      login_base_url: http://127.0.0.1:8317/v8/management\n      suspend_cooldown: 3h\n"))
	if pluginSettings.LoginBaseURL != "" || pluginSettings.SuspendCooldown != "3h" {
		t.Fatalf("settings after an invalid base: %#v", pluginSettings)
	}
	login := startTestLogin(t)
	if want := resourceBasePath + loginResourcePath + "#state=" + login.State; login.URL != want {
		t.Fatalf("sign-in URL = %q, want %q", login.URL, want)
	}
}

func TestLoginBaseURLIsDeclaredAsAConfigField(t *testing.T) {
	for _, field := range pluginConfigFields() {
		if field.Name == "login_base_url" && field.Type == pluginapi.ConfigFieldTypeString {
			return
		}
	}
	t.Fatal("login_base_url is not declared in pluginConfigFields")
}

func TestLoginBaseIsReadSafelyWhileAReconfigureWrites(t *testing.T) {
	// Run under -race: StartLogin and a host reconfigure arrive concurrently.
	// The reader calls only the accessor; going through handleLoginStart would
	// share the json package's internal sync with the writer and hide a race.
	keepLoginSettings(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			configurePlugin(configureRequest("login_base_url: http://127.0.0.1:8317\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			if base := currentLoginBaseURL(); base != "" && base != "http://127.0.0.1:8317" {
				t.Errorf("read a torn base %q", base)
				return
			}
		}
	}()
	wg.Wait()
}
