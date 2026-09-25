package query

import "testing"

func TestPseudonodeSQLClassifiesByProtocolAndWidth(t *testing.T) {
	got := PseudonodeSQL("protocol", "router_id")
	for _, want := range []string{"protocol", "router_id", "14", "16"} {
		if !contains(got, want) {
			t.Errorf("PseudonodeSQL() = %q, missing %q", got, want)
		}
	}
}

// TestPseudonodeSQLTakesColumnNames pins that the predicate can be pointed at
// ls_links' two endpoint columns, not only ls_nodes' single one. A broadcast
// LAN is modeled router -> pseudonode -> router, so both endpoints need the
// same test and neither is named router_id.
func TestPseudonodeSQLTakesColumnNames(t *testing.T) {
	got := PseudonodeSQL("protocol", "local_router_id")
	if !contains(got, "local_router_id") {
		t.Errorf("PseudonodeSQL() = %q, does not use the column it was given", got)
	}
	if contains(got, "\"router_id\"") || contains(got, " router_id ") {
		t.Errorf("PseudonodeSQL() = %q, hard-codes router_id", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
