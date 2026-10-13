import assert from "node:assert/strict";
import test from "node:test";
import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";

test("mention trigger supports Unicode and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  // Cursor inside a committed token does not re-open a mention
  assert.equal(activeMention("selected @[Finding#1 X]", 9), null);
  // The trigger only fires when the @ is preceded by a non-word char
  assert.deepEqual(activeMention("look @find trailing text", 10), { start: 5, end: 10, query: "find" });
  // A bare @ at the start of input opens a mention
  assert.deepEqual(activeMention("@find", 5), { start: 0, end: 5, query: "find" });
});

test("categories, English aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("f").categories[0].kind, "finding");
  // "find" is a prefix of the finding alias; the dropdown shows it as a category,
  // not an auto-selected kind (auto-select needs the full alias).
  assert.equal(mentionSearch("find").kind, "");
  assert.equal(mentionSearch("find").categories[0].kind, "finding");
  // Full alias match auto-selects the kind and trims the keyword
  assert.equal(mentionSearch("finding").kind, "finding");
  assert.equal(mentionSearch("finding SQL injection").query, "SQL injection");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("api GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("tokens roundtrip labels and removing one reference preserves its neighbors", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "title[1]\ndescription" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  const value = `analysis ${first} and ${second}`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  assert.equal(selected[0].label, "Finding #12 · title(1) description");
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});
