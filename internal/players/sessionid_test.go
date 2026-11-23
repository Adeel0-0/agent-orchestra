package players

import "testing"

func TestSessionIDsThatReachArgvMustBePlain(t *testing.T) {
	for _, ok := range []string{"01a1d9a8-e58b-7e22-8a89-cbe634014e35", "779a99ae-8bc5-428c-9bcb-6c0c54f66d21", "abc_123.x:y"} {
		if !ValidSessionID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-c", "--dangerously-skip-permissions", "a b", "a&b", "x\n"} {
		if ValidSessionID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := NewCLIPlayer("codex", nil, Options{Cwd: ".", SessionID: "--model=x"}); err == nil {
		t.Fatal("an option-shaped saved session id was accepted")
	}
}
