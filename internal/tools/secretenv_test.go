package tools

import "testing"

func TestSecretLookingVariablesAreHidden(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "PGPASSWORD", "DB_PASSWORD", "GITLAB_PAT",
		"AZURE_CLIENT_SECRET_VALUE", "NPM_AUTH", "AWS_CREDENTIALS", "MY_PASSWD"} {
		if !secretName(name) {
			t.Errorf("%s would reach model-run commands", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "USERPROFILE", "GOPATH", "LANG", "KEYBOARD_LAYOUT"} {
		if secretName(name) {
			t.Errorf("%s was hidden but is not a secret", name)
		}
	}
}
