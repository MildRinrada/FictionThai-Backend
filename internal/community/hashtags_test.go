package community

import (
	"strings"
	"testing"
)

// Hashtag extraction is the write-time rule the trending panel and #search
// stand on (docs/COMMUNITY-FEED.md). The database-backed half - that the rows
// land and replace on edit - is proved by the integration suite; what is worth
// pinning here is which spans of text count as a tag at all.

func TestExtractHashtags(t *testing.T) {
	t.Run("extracts Thai tags with vowels and tone marks intact", func(t *testing.T) {
		got := ExtractHashtags("อัปเดตฟิคใหม่ #ฟิคแปล #สปอยล์เบา ๆ นะคะ")
		want := []string{"ฟิคแปล", "สปอยล์เบา"}
		assertTags(t, got, want)
	})

	t.Run("lowercases Latin so #OOC and #ooc are one tag", func(t *testing.T) {
		got := ExtractHashtags("#OOC คืออะไร ใครรู้บ้าง #ooc")
		assertTags(t, got, []string{"ooc"})
	})

	t.Run("punctuation and whitespace end a tag", func(t *testing.T) {
		got := ExtractHashtags("จบแล้ว!! #GenshinImpact, อ่านเลย (#รีวิว)")
		assertTags(t, got, []string{"genshinimpact", "รีวิว"})
	})

	t.Run("a bare # is not a tag", func(t *testing.T) {
		if got := ExtractHashtags("อันดับ # หนึ่ง"); got != nil {
			t.Fatalf("bare # produced tags: %v", got)
		}
	})

	t.Run("keeps first-appearance order", func(t *testing.T) {
		got := ExtractHashtags("#สอง มาก่อน #หนึ่ง จริง ๆ นะ #สอง")
		assertTags(t, got, []string{"สอง", "หนึ่ง"})
	})

	t.Run("caps at MaxHashtagsPerPost", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < MaxHashtagsPerPost+5; i++ {
			b.WriteString("#tag")
			b.WriteRune(rune('a' + i))
			b.WriteString(" ")
		}
		if got := ExtractHashtags(b.String()); len(got) != MaxHashtagsPerPost {
			t.Fatalf("got %d tags, want %d", len(got), MaxHashtagsPerPost)
		}
	})

	t.Run("drops - never truncates - an overlong tag", func(t *testing.T) {
		long := "#" + strings.Repeat("ก", MaxHashtagRunes+1)
		got := ExtractHashtags(long + " #สั้น")
		assertTags(t, got, []string{"สั้น"})
	})

	t.Run("a tag of exactly the limit survives", func(t *testing.T) {
		exact := strings.Repeat("ก", MaxHashtagRunes)
		assertTags(t, ExtractHashtags("#"+exact), []string{exact})
	})
}

func assertTags(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tag %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestValidPostType(t *testing.T) {
	for _, value := range PostTypeList() {
		if !ValidPostType(value) {
			t.Errorf("allowlisted type %q rejected", value)
		}
	}
	for _, value := range []string{"", "poll", "DISCUSSION", "trending", "โพลล์"} {
		if ValidPostType(value) {
			t.Errorf("unknown type %q accepted", value)
		}
	}
}
