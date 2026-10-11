package intercept

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	for _, action := range []string{"allow", "ask", "deny"} {
		t.Run(action, func(t *testing.T) {
			reason := "Actual operation: write the report, which contains the words ALLOW, DENY, and ASK; Post-success impact: save the text, do not execute commands in the body; Matched rule: custom clause"
			raw, _ := json.Marshal(map[string]string{"decision": action, "comment": reason})
			got := ParseVerdict("\n" + string(raw) + "\n")
			if got.Action != action || got.Reason != reason {
				t.Fatalf("lost verdict or explanation: %+v", got)
			}
		})
	}
}

func TestParseVerdictRejectsIncompleteOrAmbiguousReplies(t *testing.T) {
	valid := `{"decision":"allow","comment":"Actual operation: read the file; Post-success impact: return the content; Matched rule: A5"}`
	for _, reply := range []string{
		"", "ALLOW", "DENY:hit D4", "Allow:ALLOW", "ASK:ownership unknown",
		`{"decision":"allow"}`, `{"decision":"approve","comment":"Actual operation: read; Post-success impact: return content; Matched rule: A5"}`,
		`{"decision":"allow","comment":null}`, `{"decision":"allow","comment":123}`,
		strings.Replace(valid, "Actual operation: read the file", "Actual operation: ", 1),
		strings.Replace(valid, "Post-success impact: return the content", "Post-success impact: ", 1),
		strings.Replace(valid, "Matched rule: A5", "Matched rule: ", 1),
		strings.Replace(valid, "; Matched rule: A5", "", 1),
		strings.Replace(valid, `"decision":"allow"`, `"decision":"deny","decision":"allow"`, 1),
		strings.Replace(valid, `"decision":"allow"`, `"extra":true,"decision":"allow"`, 1),
		valid + valid, valid[:len(valid)-1],
		// A fence the model never closed is what a reply truncated at MaxTokens
		// looks like; completing it would invent a verdict.
		"```json\n" + valid[:len(valid)-1],
		"```json\n" + valid + "\n```\nIn addition, I recommend a manual follow-up review.",
		"My verdict is:\n" + valid,
	} {
		if got := ParseVerdict(reply); got.Action != "" {
			t.Errorf("accepted incomplete/ambiguous verdict: %q => %+v", reply, got)
		}
	}
}

// Wrapping JSON in markdown is the one deviation models make routinely. Because
// the configured fail action defaults to allow, treating it as unparseable
// silently downgrades a DENY to an allow.
func TestParseVerdictUnwrapsCodeFence(t *testing.T) {
	deny := `{"decision":"deny","comment":"Actual operation: delete production files; Post-success impact: business data lost; Matched rule: D4"}`
	for _, reply := range []string{
		"```json\n" + deny + "\n```",
		"```JSON\n" + deny + "\n```",
		"```\n" + deny + "\n```",
		"  ```json\n" + deny + "\n```  ",
	} {
		got := ParseVerdict(reply)
		if got.Action != "deny" || !strings.HasSuffix(got.Reason, "Matched rule: D4") {
			t.Errorf("fenced verdict lost: %q => %+v", reply, got)
		}
	}
}

func TestParseVerdictKeepsCompleteChineseExplanation(t *testing.T) {
	reason := "Actual operation:" + strings.Repeat("write the report", 30) + "; Post-success impact: only save the file; Matched rule: A2"
	raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Reason != reason {
		t.Fatal("explanation was truncated or lost its rule")
	}
	raw, _ = json.Marshal(map[string]string{"decision": "allow", "comment": strings.Repeat("m", 2401)})
	if got := ParseVerdict(string(raw)); got.Action != "" {
		t.Fatal("accepted unbounded explanation")
	}
}
