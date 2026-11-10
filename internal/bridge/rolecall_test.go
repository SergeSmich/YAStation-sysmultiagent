package bridge

import "testing"

// The spoken grammar is small on purpose: a role answers when the phrase starts
// with a calling verb or with the role name, and never when the name is buried in
// the middle of a sentence. These cases are the contract the panel shows.

func roleCallDirectory() RoleDirectory {
	return BuildRoleDirectory([]RoleDirEntry{
		{Role: "helper"},
		{Role: "urka", Aliases: []string{"урка"}},
		{Role: "умка"},
	})
}

func TestParseRoleCall(t *testing.T) {
	dir := roleCallDirectory()
	cases := []struct {
		text     string
		ok       bool
		role     string
		kind     string
		question string
	}{
		{text: "Алиса, позови helper", ok: true, role: "helper", kind: RoleCallGreeting},
		{text: "позови helper", ok: true, role: "helper", kind: RoleCallGreeting},
		{text: "вызови urka", ok: true, role: "urka", kind: RoleCallGreeting},
		{text: "подзови умку", ok: true, role: "умка", kind: RoleCallGreeting},
		{text: "обратись к урке", ok: true, role: "urka", kind: RoleCallGreeting},
		{text: "спроси urka, который час", ok: true, role: "urka", kind: RoleCallQuestion, question: "который час"},
		{text: "спроси у умки, что нового", ok: true, role: "умка", kind: RoleCallQuestion, question: "что нового"},
		{text: "позови helper, который час", ok: true, role: "helper", kind: RoleCallQuestion, question: "который час"},
		{text: "helper, ты здесь?", ok: true, role: "helper", kind: RoleCallGreeting},
		{text: "helper, ты тут", ok: true, role: "helper", kind: RoleCallGreeting},
		{text: "умка, ау", ok: true, role: "умка", kind: RoleCallGreeting},
		{text: "helper", ok: true, role: "helper", kind: RoleCallGreeting},
		{text: "helper?", ok: true, role: "helper", kind: RoleCallGreeting},
		{text: "helper, который час", ok: true, role: "helper", kind: RoleCallQuestion, question: "который час"},
		{text: "умка — как дела", ok: true, role: "умка", kind: RoleCallQuestion, question: "как дела"},
		// A name in the middle of a phrase is never a call.
		{text: "включи свет, helper"},
		{text: "расскажи про helper"},
		{text: "Алиса, включи свет"},
		// A verb without a name, an unknown name, or a name run into a word.
		{text: "позови"},
		{text: "позови соседа"},
		{text: "helper как дела"},
		{text: "helperka, ты здесь"},
	}
	for _, tc := range cases {
		call, ok := ParseRoleCall(tc.text, dir)
		if ok != tc.ok {
			t.Errorf("%q: ok=%v, want %v (call=%+v)", tc.text, ok, tc.ok, call)
			continue
		}
		if !ok {
			continue
		}
		if call.Role != tc.role || call.Kind != tc.kind || call.Question != tc.question {
			t.Errorf("%q: got %+v, want role=%s kind=%s question=%q", tc.text, call, tc.role, tc.kind, tc.question)
		}
	}
}

// TestRoleCallEmptyDirectory: without callable roles nothing resolves, whatever
// the phrase says.
func TestRoleCallEmptyDirectory(t *testing.T) {
	dir := BuildRoleDirectory(nil)
	if _, ok := ParseRoleCall("позови helper", dir); ok {
		t.Fatal("an empty directory resolved a role")
	}
}

// TestSpokenForms covers the case endings: a Cyrillic name declines by its final
// vowel, a Latin one is used as written.
func TestSpokenForms(t *testing.T) {
	forms := spokenForms("умка")
	want := []string{"умка", "умку", "умке", "умки", "умкой"}
	for _, form := range want {
		found := false
		for _, have := range forms {
			if have == form {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("spokenForms(умка) lacks %q: %v", form, forms)
		}
	}
	if got := spokenForms("urka"); len(got) != 1 || got[0] != "urka" {
		t.Fatalf("a Latin name must not be declined: %v", got)
	}
	if got := spokenForms("УМКА"); len(got) == 0 || got[0] != "умка" {
		t.Fatalf("a name is folded to lower case: %v", got)
	}
}
