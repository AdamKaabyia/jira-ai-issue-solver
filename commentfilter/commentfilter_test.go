package commentfilter_test

import (
	"testing"
	"time"

	"jira-ai-issue-solver/commentfilter"
	"jira-ai-issue-solver/models"
)

// --- Filter ---

func TestFilter_RemovesIgnoredUsers(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "packit-as-a-service[bot]"}, Body: "/packit build"},
		{ID: 3, Author: models.Author{Username: "reviewer2"}, Body: "Also fix that"},
	}

	cfg := commentfilter.Config{
		BotUsername:      "ai-bot",
		IgnoredUsernames: []string{"packit-as-a-service"},
	}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 2 {
		t.Fatalf("got %d comments, want 2", len(result))
	}
	if result[0].ID != 1 || result[1].ID != 3 {
		t.Errorf("result IDs = [%d, %d], want [1, 3]", result[0].ID, result[1].ID)
	}
}

func TestFilter_RemovesKnownBotReplyingToOurBot(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Addressed", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Also check X", InReplyTo: 2},
	}

	cfg := commentfilter.Config{
		BotUsername:       "ai-bot",
		KnownBotUsernames: []string{"coderabbitai"},
	}

	result := commentfilter.Filter(comments, cfg)

	// Comment 3 should be removed (known bot replying to our bot).
	// Comment 2 (our bot) should be preserved.
	if len(result) != 2 {
		t.Fatalf("got %d comments, want 2", len(result))
	}
	if result[0].ID != 1 || result[1].ID != 2 {
		t.Errorf("result IDs = [%d, %d], want [1, 2]", result[0].ID, result[1].ID)
	}
}

func TestFilter_KeepsKnownBotTopLevelComment(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Initial review"},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "Agreed"},
	}

	cfg := commentfilter.Config{
		BotUsername:       "ai-bot",
		KnownBotUsernames: []string{"coderabbitai"},
	}

	result := commentfilter.Filter(comments, cfg)

	// Top-level known bot comment should be kept.
	if len(result) != 2 {
		t.Fatalf("got %d comments, want 2", len(result))
	}
}

func TestFilter_KnownBotReplyToMissingParent_Skipped(t *testing.T) {
	comments := []models.PRComment{
		{ID: 10, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Follow-up", InReplyTo: 999},
	}

	cfg := commentfilter.Config{
		BotUsername:       "ai-bot",
		KnownBotUsernames: []string{"coderabbitai"},
	}

	result := commentfilter.Filter(comments, cfg)

	// Defensive skip: parent not in the set.
	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (defensive skip)", len(result))
	}
}

func TestFilter_KnownBotReplyToNonBotParent_Kept(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Question"},
		{ID: 2, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Answer", InReplyTo: 1},
	}

	cfg := commentfilter.Config{
		BotUsername:       "ai-bot",
		KnownBotUsernames: []string{"coderabbitai"},
	}

	result := commentfilter.Filter(comments, cfg)

	// Known bot replying to a reviewer (not our bot) should be kept.
	if len(result) != 2 {
		t.Fatalf("got %d comments, want 2", len(result))
	}
}

func TestFilter_ThreadDepthExceeded(t *testing.T) {
	// Thread: reviewer -> bot -> reviewer -> bot -> reviewer
	// Thread depth at comment 5 is 2 (two bot appearances in chain).
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Addressed", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Not quite", InReplyTo: 2},
		{ID: 4, Author: models.Author{Username: "ai-bot"}, Body: "Fixed again", InReplyTo: 3},
		{ID: 5, Author: models.Author{Username: "reviewer"}, Body: "Still wrong", InReplyTo: 4},
	}

	cfg := commentfilter.Config{
		BotUsername:    "ai-bot",
		MaxThreadDepth: 2,
	}

	result := commentfilter.Filter(comments, cfg)

	// Comment 5 has depth 2 (bot at IDs 2 and 4), which equals max.
	// Comments 1 and 3 have depth < 2.
	// Bot comments (2, 4) are preserved.
	ids := extractIDs(result)
	assertIDs(t, ids, []int64{1, 2, 3, 4})
}

func TestFilter_ThreadDepthNotExceeded(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Addressed", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Thanks", InReplyTo: 2},
	}

	cfg := commentfilter.Config{
		BotUsername:    "ai-bot",
		MaxThreadDepth: 5,
	}

	result := commentfilter.Filter(comments, cfg)

	// Depth at comment 3 is 1 (bot at ID 2), which is < 5.
	if len(result) != 3 {
		t.Fatalf("got %d comments, want 3", len(result))
	}
}

func TestFilter_ThreadDepthDisabledWhenZero(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Again", InReplyTo: 2},
		{ID: 4, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 3},
		{ID: 5, Author: models.Author{Username: "reviewer"}, Body: "Again", InReplyTo: 4},
	}

	cfg := commentfilter.Config{
		BotUsername:    "ai-bot",
		MaxThreadDepth: 0, // disabled
	}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 5 {
		t.Fatalf("got %d comments, want 5 (depth disabled)", len(result))
	}
}

func TestFilter_PreservesBotOwnComments(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "ai-bot"}, Body: "I started"},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 3, Author: models.Author{Username: "ai-bot"}, Body: "Addressed", InReplyTo: 2},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 3 {
		t.Fatalf("got %d comments, want 3 (bot comments preserved)", len(result))
	}
}

func TestFilter_EmptyInput(t *testing.T) {
	result := commentfilter.Filter(nil, commentfilter.Config{BotUsername: "ai-bot"})

	if result == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0", len(result))
	}
}

func TestFilter_UsernameNormalization_CaseInsensitive(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "PACKIT"}, Body: "build"},
	}

	cfg := commentfilter.Config{
		BotUsername:      "ai-bot",
		IgnoredUsernames: []string{"packit"},
	}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (case-insensitive ignore)", len(result))
	}
}

func TestFilter_UsernameNormalization_BotSuffix(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "dependabot[bot]"}, Body: "bump"},
	}

	cfg := commentfilter.Config{
		BotUsername:      "ai-bot",
		IgnoredUsernames: []string{"dependabot"},
	}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 ([bot] suffix normalized)", len(result))
	}
}

func TestFilter_CycleInCommentChain(t *testing.T) {
	// Malformed data: comment 1 and 2 reference each other.
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "ai-bot"}, Body: "A", InReplyTo: 2},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "B", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Fix", InReplyTo: 1},
	}

	cfg := commentfilter.Config{
		BotUsername:    "ai-bot",
		MaxThreadDepth: 10,
	}

	// Should not hang or panic.
	result := commentfilter.Filter(comments, cfg)

	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestFilter_CombinedRules(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Addressed", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Loop", InReplyTo: 2},
		{ID: 4, Author: models.Author{Username: "packit[bot]"}, Body: "/build"},
		{ID: 5, Author: models.Author{Username: "reviewer2"}, Body: "Update docs"},
		{ID: 6, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot ignore\nHint for CodeRabbit"},
	}

	cfg := commentfilter.Config{
		BotUsername:       "ai-bot",
		IgnoredUsernames:  []string{"packit"},
		KnownBotUsernames: []string{"coderabbitai"},
		MaxThreadDepth:    5,
	}

	result := commentfilter.Filter(comments, cfg)

	// Kept: 1 (reviewer), 2 (our bot), 5 (reviewer2)
	// Removed: 3 (known bot replying to our bot), 4 (ignored), 6 (ignore directive)
	ids := extractIDs(result)
	assertIDs(t, ids, []int64{1, 2, 5})
}

// --- Slash-command-only filtering ---

func TestFilter_RemovesSlashCommandOnlyComments(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "/lgtm\r\n/approve"},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "Looks good, just one nit"},
		{ID: 3, Author: models.Author{Username: "reviewer2"}, Body: "/lgtm"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	ids := extractIDs(result)
	assertIDs(t, ids, []int64{2})
}

func TestFilter_KeepsSlashCommandWithSurroundingText(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Looks good!\n/lgtm"},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "/lgtm\nGreat work"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 2 {
		t.Fatalf("got %d comments, want 2 (mixed content kept)", len(result))
	}
}

func TestFilter_KeepsEmptyBodyComment(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: ""},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "   \n\n  "},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 2 {
		t.Fatalf("got %d comments, want 2 (empty bodies are not slash commands)", len(result))
	}
}

func TestFilter_RemovesSlashCommandWithArguments(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "/assign @user\n/priority critical"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (slash commands with args)", len(result))
	}
}

func TestFilter_RemovesSlashCommandWithBlankLines(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "/lgtm\n\n/approve\n"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (slash commands with blank lines between)", len(result))
	}
}

// --- @botname ignore directive ---

func TestFilter_RemovesBotIgnoreDirective(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot ignore\n\nThis is for humans only."},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "Please fix this bug"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	ids := extractIDs(result)
	assertIDs(t, ids, []int64{2})
}

func TestFilter_BotIgnoreDirective_AtEndOfComment(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "This hint is for CodeRabbit.\n\n@ai-bot ignore"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (directive at end of comment)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_InlineInComment(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Some context @ai-bot ignore more text here"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (directive inline)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_CaseInsensitive(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "@AI-BOT Ignore\nNot for the bot"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (case-insensitive match)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_WithBotSuffix(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot[bot] ignore\nNot for the bot"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (with [bot] suffix)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_WordBoundary(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot ignoring this for now"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	// "ignoring" should NOT match — word boundary prevents it.
	if len(result) != 1 {
		t.Fatalf("got %d comments, want 1 ('ignoring' should not match)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_NewlineBetweenMentionAndIgnore(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot\nignore the linter warnings on line 42"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	// Newline between mention and "ignore" should NOT match — only
	// horizontal whitespace (space/tab) is accepted.
	if len(result) != 1 {
		t.Fatalf("got %d comments, want 1 (newline between mention and ignore should not match)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_MentionInsideLargerToken(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "contact-foo@ai-bot ignore this"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	// @ inside a larger token (e.g., email-like) should NOT match.
	if len(result) != 1 {
		t.Fatalf("got %d comments, want 1 (mention inside larger token should not match)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_DoesNotAffectBotOwnComments(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "ai-bot"}, Body: "@ai-bot ignore\nSome bot message"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	result := commentfilter.Filter(comments, cfg)

	// Bot's own comments are always preserved regardless of content.
	if len(result) != 1 {
		t.Fatalf("got %d comments, want 1 (bot's own comments preserved)", len(result))
	}
}

func TestFilter_BotIgnoreDirective_ConfigUsernameWithBotSuffix(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "@my-bot ignore\nHuman-only comment"},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "@my-bot[bot] ignore\nAlso human-only"},
	}

	// Config username includes [bot] suffix.
	cfg := commentfilter.Config{BotUsername: "my-bot[bot]"}

	result := commentfilter.Filter(comments, cfg)

	// Both should be filtered: normalizeUsername strips [bot] from config,
	// and the regex allows an optional [bot] in the mention.
	if len(result) != 0 {
		t.Fatalf("got %d comments, want 0 (config username with [bot] suffix)", len(result))
	}
}

func TestHasNewActionable_FalseWhenOnlyBotIgnoreDirective(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot ignore\nThis is for humans"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: only comment has ignore directive")
	}
}

func TestHasNewActionable_FalseWhenOnlySlashCommands(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "/lgtm\n/approve"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: only slash-command comments")
	}
}

// --- HasNewActionable ---

func TestHasNewActionable_TrueWhenNewComments(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if !commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected true: unaddressed reviewer comment")
	}
}

func TestHasNewActionable_FalseWhenAllAddressed(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: all comments addressed")
	}
}

func TestHasNewActionable_FalseWhenEmpty(t *testing.T) {
	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if commentfilter.HasNewActionable(nil, cfg) {
		t.Error("expected false: no comments")
	}
}

func TestHasNewActionable_FalseWhenOnlyBotComments(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "ai-bot"}, Body: "Status update"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: only bot comments")
	}
}

func TestHasNewActionable_FalseWhenOnlyIgnoredUsers(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "packit[bot]"}, Body: "/build"},
	}

	cfg := commentfilter.Config{
		BotUsername:      "ai-bot",
		IgnoredUsernames: []string{"packit"},
	}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: only ignored users")
	}
}

func TestHasNewActionable_TrueWhenMixedAddressedAndNew(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Old"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "New"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if !commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected true: comment 3 is new")
	}
}

func TestHasNewActionable_FalseWhenFilteredByLoopPrevention(t *testing.T) {
	// Only "new" comment is from a known bot replying to our bot.
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Also check", InReplyTo: 2},
	}

	cfg := commentfilter.Config{
		BotUsername:       "ai-bot",
		KnownBotUsernames: []string{"coderabbitai"},
	}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: only actionable comment is a bot loop")
	}
}

// --- AddressedMarker ---

func TestAddressedMarker_Format(t *testing.T) {
	got := commentfilter.AddressedMarker(12345)
	want := "<!-- addressed: 12345 -->"
	if got != want {
		t.Errorf("AddressedMarker(12345) = %q, want %q", got, want)
	}
}

// --- BotRepliedTo ---

func TestBotRepliedTo_ReviewCommentViaInReplyTo(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
	}

	replied := commentfilter.BotRepliedTo(comments, "ai-bot")

	if !replied[1] {
		t.Error("expected comment 1 to be in replied set (via InReplyTo)")
	}
	if replied[2] {
		t.Error("comment 2 is the bot's own reply, should not be in set")
	}
}

func TestBotRepliedTo_ConversationCommentViaMarker(t *testing.T) {
	marker := commentfilter.AddressedMarker(100)
	comments := []models.PRComment{
		{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Please update docs"},
		{ID: 200, Author: models.Author{Username: "ai-bot"}, Body: "Updated.\n" + marker},
	}

	replied := commentfilter.BotRepliedTo(comments, "ai-bot")

	if !replied[100] {
		t.Error("expected comment 100 to be in replied set (via marker)")
	}
}

func TestBotRepliedTo_IgnoresNonBotMarkers(t *testing.T) {
	marker := commentfilter.AddressedMarker(100)
	comments := []models.PRComment{
		{ID: 50, Author: models.Author{Username: "reviewer"}, Body: "Tricky: " + marker},
	}

	replied := commentfilter.BotRepliedTo(comments, "ai-bot")

	if replied[100] {
		t.Error("marker in non-bot comment should be ignored")
	}
}

func TestBotRepliedTo_BothMechanisms(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)
	marker := commentfilter.AddressedMarker(10)
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Review comment",
			IsReviewComment: true, Timestamp: t0},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done",
			InReplyTo: 1, IsReviewComment: true, Timestamp: t0.Add(time.Minute)},
		{ID: 10, Author: models.Author{Username: "reviewer"}, Body: "Conversation comment",
			Timestamp: t0.Add(2 * time.Minute)},
		{ID: 20, Author: models.Author{Username: "ai-bot"}, Body: "Updated.\n" + marker,
			Timestamp: t0.Add(3 * time.Minute)},
	}

	replied := commentfilter.BotRepliedTo(comments, "ai-bot")

	if !replied[1] {
		t.Error("review comment 1 should be addressed via InReplyTo")
	}
	if !replied[10] {
		t.Error("conversation comment 10 should be addressed via marker")
	}
}

// --- HasNewActionable with conversation comments ---

func TestHasNewActionable_FalseWhenConversationCommentAddressedViaMarker(t *testing.T) {
	marker := commentfilter.AddressedMarker(1)
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Update docs"},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done.\n" + marker},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: conversation comment addressed via marker")
	}
}

func TestHasNewActionable_TrueWhenConversationCommentNotAddressed(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Update docs"},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if !commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected true: conversation comment not addressed")
	}
}

// --- Flat review thread tests (GitHub normalises InReplyTo to thread root) ---

func TestFilter_ThreadDepthExceeded_FlatReviewThread(t *testing.T) {
	// Flat review thread: root comment + 3 bot replies + 1 reviewer follow-up.
	// All replies point to root (ID 1), simulating GitHub's normalisation.
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this", IsReviewComment: true},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1, IsReviewComment: true},
		{ID: 3, Author: models.Author{Username: "ai-bot"}, Body: "Unable", InReplyTo: 1, IsReviewComment: true},
		{ID: 4, Author: models.Author{Username: "ai-bot"}, Body: "Unable", InReplyTo: 1, IsReviewComment: true},
		{ID: 5, Author: models.Author{Username: "reviewer"}, Body: "Still broken", InReplyTo: 1, IsReviewComment: true},
	}

	cfg := commentfilter.Config{
		BotUsername:    "ai-bot",
		MaxThreadDepth: 2, // 3 bot replies >= 2 → filter follow-ups
	}

	result := commentfilter.Filter(comments, cfg)

	// Comment 5 (reviewer follow-up) should be filtered: 3 bot replies >= MaxThreadDepth 2.
	// Comment 1 (root) has 0 bot replies in its ancestry (it IS the root) → kept.
	// Bot comments (2, 3, 4) are always preserved.
	ids := extractIDs(result)
	assertIDs(t, ids, []int64{1, 2, 3, 4})
}

func TestFilter_ThreadDepthNotExceeded_FlatReviewThread(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this", IsReviewComment: true},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1, IsReviewComment: true},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "One more thing", InReplyTo: 1, IsReviewComment: true},
	}

	cfg := commentfilter.Config{
		BotUsername:    "ai-bot",
		MaxThreadDepth: 5, // 1 bot reply < 5 → nothing filtered
	}

	result := commentfilter.Filter(comments, cfg)

	if len(result) != 3 {
		t.Fatalf("got %d comments, want 3 (depth not exceeded)", len(result))
	}
}

func TestFilter_ThreadDepth_FlatReviewThread_RootNotFiltered(t *testing.T) {
	// Root comment (InReplyTo=0) should use its own ID as thread root.
	// Even with many bot replies, the root itself should not be filtered.
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this", IsReviewComment: true},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1, IsReviewComment: true},
		{ID: 3, Author: models.Author{Username: "ai-bot"}, Body: "Unable", InReplyTo: 1, IsReviewComment: true},
	}

	cfg := commentfilter.Config{
		BotUsername:    "ai-bot",
		MaxThreadDepth: 1,
	}

	result := commentfilter.Filter(comments, cfg)

	// Root comment 1 should NOT be filtered (its thread root is itself,
	// and the bot replies point to 1, not to themselves).
	ids := extractIDs(result)
	assertIDs(t, ids, []int64{1, 2, 3})
}

func TestBotRepliedTo_FlatReviewThread_FollowUpAddressed(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)

	comments := []models.PRComment{
		{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Fix this",
			IsReviewComment: true, Timestamp: t0},
		{ID: 200, Author: models.Author{Username: "reviewer"}, Body: "Also check that",
			IsReviewComment: true, InReplyTo: 100, Timestamp: t0.Add(time.Minute)},
		{ID: 300, Author: models.Author{Username: "ai-bot"}, Body: "Unable",
			IsReviewComment: true, InReplyTo: 100, Timestamp: t0.Add(2 * time.Minute)},
	}

	replied := commentfilter.BotRepliedTo(comments, "ai-bot")

	if !replied[100] {
		t.Error("root comment 100 should be replied-to via InReplyTo")
	}
	if !replied[200] {
		t.Error("follow-up 200 should be replied-to: bot reply (T+2m) is after follow-up (T+1m)")
	}
}

func TestBotRepliedTo_FlatReviewThread_NewFollowUpNotAddressed(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)

	comments := []models.PRComment{
		{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Fix this",
			IsReviewComment: true, Timestamp: t0},
		{ID: 200, Author: models.Author{Username: "ai-bot"}, Body: "Done",
			IsReviewComment: true, InReplyTo: 100, Timestamp: t0.Add(time.Minute)},
		{ID: 300, Author: models.Author{Username: "reviewer"}, Body: "New feedback",
			IsReviewComment: true, InReplyTo: 100, Timestamp: t0.Add(2 * time.Minute)},
	}

	replied := commentfilter.BotRepliedTo(comments, "ai-bot")

	if !replied[100] {
		t.Error("root comment 100 should be replied-to")
	}
	if replied[300] {
		t.Error("follow-up 300 should NOT be replied-to: it's newer than bot's reply")
	}
}

func TestBotRepliedTo_FlatReviewThread_SameTimestampTreatedAsAddressed(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)

	comments := []models.PRComment{
		{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Fix this",
			IsReviewComment: true, Timestamp: t0},
		{ID: 200, Author: models.Author{Username: "ai-bot"}, Body: "Unable",
			IsReviewComment: true, InReplyTo: 100, Timestamp: t0.Add(time.Minute)},
		{ID: 300, Author: models.Author{Username: "reviewer"}, Body: "Still wrong",
			IsReviewComment: true, InReplyTo: 100, Timestamp: t0.Add(time.Minute)},
	}

	replied := commentfilter.BotRepliedTo(comments, "ai-bot")

	if !replied[300] {
		t.Error("same-timestamp follow-up should be treated as addressed (safe direction to prevent loops)")
	}
}

func TestHasNewActionable_FalseWhenFlatReviewThreadFullyAddressed(t *testing.T) {
	// Reproduces the PR #3123 scenario: reviewer posts follow-ups, bot
	// replies with "unable". All follow-ups are older than the bot's
	// reply, so nothing should be actionable.
	t0 := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)

	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this",
			IsReviewComment: true, Timestamp: t0},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Addressed in abc123",
			IsReviewComment: true, InReplyTo: 1, Timestamp: t0.Add(time.Minute)},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Not quite right",
			IsReviewComment: true, InReplyTo: 1, Timestamp: t0.Add(2 * time.Minute)},
		{ID: 4, Author: models.Author{Username: "reviewer"}, Body: "Also wrong here",
			IsReviewComment: true, InReplyTo: 1, Timestamp: t0.Add(3 * time.Minute)},
		{ID: 5, Author: models.Author{Username: "ai-bot"}, Body: "Unable",
			IsReviewComment: true, InReplyTo: 1, Timestamp: t0.Add(4 * time.Minute)},
		{ID: 6, Author: models.Author{Username: "ai-bot"}, Body: "Unable",
			IsReviewComment: true, InReplyTo: 1, Timestamp: t0.Add(4 * time.Minute)},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected false: all follow-ups are older than the bot's latest reply")
	}
}

func TestHasNewActionable_TrueWhenNewFollowUpAfterBotReply(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)

	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this",
			IsReviewComment: true, Timestamp: t0},
		{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done",
			IsReviewComment: true, InReplyTo: 1, Timestamp: t0.Add(time.Minute)},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "New issue found",
			IsReviewComment: true, InReplyTo: 1, Timestamp: t0.Add(2 * time.Minute)},
	}

	cfg := commentfilter.Config{BotUsername: "ai-bot"}

	if !commentfilter.HasNewActionable(comments, cfg) {
		t.Error("expected true: follow-up at T+2m is newer than bot reply at T+1m")
	}
}

// --- helpers ---

func extractIDs(comments []models.PRComment) []int64 {
	ids := make([]int64, len(comments))
	for i, c := range comments {
		ids[i] = c.ID
	}
	return ids
}

func assertIDs(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got IDs %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got IDs %v, want %v", got, want)
		}
	}
}

// --- IsSyncCommand ---

func TestIsSyncCommand(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		botUsername string
		want        bool
	}{
		{
			name:        "basic match",
			body:        "@ai-bot sync",
			botUsername: "ai-bot",
			want:        true,
		},
		{
			name:        "at start with trailing text",
			body:        "@ai-bot sync\n\nPlease pull main.",
			botUsername: "ai-bot",
			want:        true,
		},
		{
			name:        "at end of comment",
			body:        "CI is broken, can you\n@ai-bot sync",
			botUsername: "ai-bot",
			want:        true,
		},
		{
			name:        "inline in comment",
			body:        "Hey @ai-bot sync please",
			botUsername: "ai-bot",
			want:        true,
		},
		{
			name:        "case insensitive",
			body:        "@AI-BOT Sync",
			botUsername: "ai-bot",
			want:        true,
		},
		{
			name:        "with [bot] suffix",
			body:        "@ai-bot[bot] sync",
			botUsername: "ai-bot",
			want:        true,
		},
		{
			name:        "config username has [bot] suffix",
			body:        "@my-bot sync",
			botUsername: "my-bot[bot]",
			want:        true,
		},
		{
			name:        "word boundary prevents syncing",
			body:        "@ai-bot syncing main",
			botUsername: "ai-bot",
			want:        false,
		},
		{
			name:        "newline between mention and sync",
			body:        "@ai-bot\nsync",
			botUsername: "ai-bot",
			want:        false,
		},
		{
			name:        "mention inside larger token",
			body:        "contact-foo@ai-bot sync",
			botUsername: "ai-bot",
			want:        false,
		},
		{
			name:        "no match without mention",
			body:        "sync main into this branch",
			botUsername: "ai-bot",
			want:        false,
		},
		{
			name:        "empty body",
			body:        "",
			botUsername: "ai-bot",
			want:        false,
		},
		{
			name:        "wrong bot username",
			body:        "@other-bot sync",
			botUsername: "ai-bot",
			want:        false,
		},
		{
			name:        "tab between mention and sync",
			body:        "@ai-bot\tsync",
			botUsername: "ai-bot",
			want:        true,
		},
		{
			name:        "multiple spaces between mention and sync",
			body:        "@ai-bot   sync",
			botUsername: "ai-bot",
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commentfilter.IsSyncCommand(tt.body, tt.botUsername)
			if got != tt.want {
				t.Errorf("IsSyncCommand(%q, %q) = %v, want %v",
					tt.body, tt.botUsername, got, tt.want)
			}
		})
	}
}
