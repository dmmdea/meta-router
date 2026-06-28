package main

import (
	"strings"
	"testing"
)

func TestOffloadNudge_VerbPlusBlockFires(t *testing.T) {
	// An offload verb + a substantial multi-line pasted block.
	prompt := "Summarize the following log:\n" + strings.Repeat("line of pasted text\n", 20)
	if !offloadNudge(prompt) {
		t.Fatal("verb + large block: want true")
	}
}

func TestOffloadNudge_VerbOnlyShortDoesNotFire(t *testing.T) {
	if offloadNudge("summarize our discussion") {
		t.Fatal("verb but no block: want false")
	}
}

func TestOffloadNudge_BlockOnlyNoVerbDoesNotFire(t *testing.T) {
	prompt := "Here is some context for our chat:\n" + strings.Repeat("background detail\n", 20)
	if offloadNudge(prompt) {
		t.Fatal("block but no offload verb: want false")
	}
}

func TestOffloadNudge_PunctuationAndCaseStripped(t *testing.T) {
	prompt := "CLASSIFY: " + strings.Repeat("ticket text here ", 40) // long single line >= 400 chars
	if !offloadNudge(prompt) {
		t.Fatal("verb with trailing colon + long block: want true")
	}
}

func TestOffloadNudge_LongLineCountsAsBlock(t *testing.T) {
	prompt := "extract the fields from " + strings.Repeat("x", 500) // one long line, no newlines
	if !offloadNudge(prompt) {
		t.Fatal("verb + >=400 char input: want true")
	}
}

func TestOffloadNudge_BuriedVerbInConversationDoesNotFire(t *testing.T) {
	// A long conversational prompt whose offload verb is buried mid-sentence is
	// chat that merely mentions the word, not an imperative task — must not fire.
	prompt := "Can you help me think through whether we should summarize our quarterly strategy and weigh the many tradeoffs involved here before we commit to anything substantial " + strings.Repeat("and so on ", 30)
	if offloadNudge(prompt) {
		t.Fatal("buried verb in long conversation: want false")
	}
}

func TestAppendNudge_EmptyContext(t *testing.T) {
	if got := appendNudge(""); got != offloadNudgeText {
		t.Fatalf("empty ctx: want bare nudge, got %q", got)
	}
}

func TestAppendNudge_NonEmptyContext(t *testing.T) {
	got := appendNudge("skills here")
	if !strings.HasPrefix(got, "skills here") || !strings.Contains(got, offloadNudgeText) {
		t.Fatalf("non-empty ctx: want ctx + nudge, got %q", got)
	}
}
