package util

import "testing"

func TestQuoteFTS5_EscapesInnerQuotes(t *testing.T) {
	got := QuoteFTS5(`Bob"s Entity`)
	want := `"Bob""s Entity"`
	if got != want {
		t.Errorf("QuoteFTS5 = %q, want %q", got, want)
	}
}

func TestQuoteFTS5_PlainName(t *testing.T) {
	got := QuoteFTS5("Alice")
	want := `"Alice"`
	if got != want {
		t.Errorf("QuoteFTS5 = %q, want %q", got, want)
	}
}

func TestQuoteFTS5Query_MultiWordEachTokenQuoted(t *testing.T) {
	got := QuoteFTS5Query("hello world AND")
	want := `"hello" "world" "AND"`
	if got != want {
		t.Errorf("QuoteFTS5Query = %q, want %q", got, want)
	}
}

func TestQuoteFTS5Query_EmptyInput(t *testing.T) {
	got := QuoteFTS5Query("   ")
	want := `""`
	if got != want {
		t.Errorf("QuoteFTS5Query(whitespace) = %q, want %q", got, want)
	}
}

func TestQuoteFTS5Query_SyntaxCharactersNeutralized(t *testing.T) {
	// 原始输入若直接拼进 MATCH ?，"OR"/"*"/引号 都会被 FTS5 当语法解析，
	// 转义后必须整体变成安全的字面量 token。
	got := QuoteFTS5Query(`foo* OR "bar"`)
	want := `"foo*" "OR" """bar"""`
	if got != want {
		t.Errorf("QuoteFTS5Query = %q, want %q", got, want)
	}
}
