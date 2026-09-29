package dbstate

import "testing"

func TestStageCommitDiscard(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	id := "00000000deadbeef"
	if err := WritePassword(id, "old"); err != nil {
		t.Fatal(err)
	}
	// Preparada pero no confirmada: la vigente no cambia.
	if err := StagePassword(id, "new"); err != nil {
		t.Fatal(err)
	}
	if pw, _ := ReadPassword(id); pw != "old" {
		t.Fatalf("staged password leaked into the live one: %q", pw)
	}
	DiscardStaged(id)
	if err := CommitStaged(id); err == nil {
		t.Fatal("commit after discard must fail")
	}
	if pw, _ := ReadPassword(id); pw != "old" {
		t.Fatalf("got %q", pw)
	}
	if err := StagePassword(id, "new"); err != nil {
		t.Fatal(err)
	}
	if err := CommitStaged(id); err != nil {
		t.Fatal(err)
	}
	if pw, _ := ReadPassword(id); pw != "new" {
		t.Fatalf("got %q", pw)
	}
	if err := StagePassword(id, "a\nb"); err == nil {
		t.Fatal("multi-line password accepted")
	}
}
