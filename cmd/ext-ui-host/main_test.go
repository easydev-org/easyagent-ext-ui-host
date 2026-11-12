package main

import "testing"

/**
 * PANEL_REPOS parsing.
 *
 * THE SPELLING TOLERANCE IS THE POINT: this value is typed into a YAML manifest by hand, and a wrong
 * key would otherwise be a silent no-op — a host that thinks it manages three panels and manages none.
 * Both `extId` and `ext_id` are accepted for that reason.
 */
func TestParsePanelRepos(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"whitespace", "   \n ", 0},
		{"camelCase", `[{"extId":"gitdev","repo":"o/n"}]`, 1},
		{"snake_case", `[{"ext_id":"gitdev","repo":"o/n"}]`, 1},
		{"two", `[{"extId":"a","repo":"o/a"},{"extId":"b","repo":"o/b"}]`, 2},
		// A ROW WITH NO REPO CAN NEVER INSTALL ANYTHING, so it is dropped rather than kept to fail
		// later with a worse message.
		{"missing repo", `[{"extId":"a","repo":"o/a"},{"extId":"b"}]`, 1},
		{"missing id", `[{"repo":"o/a"}]`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePanelRepos(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != c.want {
				t.Fatalf("want %d, got %d (%+v)", c.want, len(got), got)
			}
		})
	}
}

func TestParsePanelReposRejectsMalformedJSON(t *testing.T) {
	// FATAL AT STARTUP, unlike a malformed contribution inside a panel. This value decides what gets
	// installed and executed in the console's origin, so starting with it half-understood would mean a
	// deployment that believes it manages panels it does not.
	if _, err := parsePanelRepos(`{not json`); err == nil {
		t.Fatal("malformed JSON should be reported, not ignored")
	}
}
