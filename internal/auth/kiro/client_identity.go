package kiro

import (
	"fmt"
	"runtime"
	"strings"
)

// clientVersion is the Kiro CLI release this plugin reports. Release builds
// replace it with -ldflags -X. The default is deliberately not a real version:
// the linker drops an -X that names a missing symbol without reporting it, and
// a default equal to the stamped value would let that failure pass a build
// check that greps the binary for it.
var clientVersion = "dev"

func ClientVersion() string {
	version := strings.TrimSpace(clientVersion)
	if version == "" {
		return "unknown"
	}
	return version
}

// DesktopAuthUserAgent is the User-Agent kiro-cli sends to the Kiro desktop
// auth service (prod.us-east-1.auth.desktop.kiro.dev). Without it Go sends
// "Go-http-client/1.1". Reference: kirodotdev/KiroCrew,
// src/kiro_crew/auth/login/endpoints.py (USER_AGENT).
const DesktopAuthUserAgent = "Kiro-CLI"

// ClientUserAgent identifies the plugin as a Kiro CLI-compatible client. It
// reports the real build platform and does not create a synthetic device ID.
func ClientUserAgent() string {
	return fmt.Sprintf(
		"KiroCLI/%s os/%s arch/%s app/AmazonQ-For-CLI",
		ClientVersion(), runtime.GOOS, runtime.GOARCH,
	)
}

func ClientAWSUserAgent(api string) string {
	api = strings.TrimSpace(api)
	if api == "" {
		api = "codewhispererstreaming"
	}
	return fmt.Sprintf(
		"aws-sdk-rust/1.3.9 ua/2.1 api/%s/1.0.0 os/%s arch/%s lang/rust m/E app/AmazonQ-For-CLI md/appVersion#%s",
		api, runtime.GOOS, runtime.GOARCH, ClientVersion(),
	)
}
