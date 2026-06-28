package retrievers

import "testing"

func TestParseEmbedResponse_PairsByIndex(t *testing.T) {
	// data deliberately out of order: index 1 before index 0
	body := []byte(`{"data":[{"index":1,"embedding":[9,9]},{"index":0,"embedding":[1,1]}]}`)
	vs, err := parseEmbedResponse(body, 2)
	if err != nil {
		t.Fatal(err)
	}
	if vs[0][0] != 1 || vs[1][0] != 9 {
		t.Fatalf("paired by position, not index: %v", vs)
	}
}

func TestParseEmbedResponse_CountMismatch(t *testing.T) {
	body := []byte(`{"data":[{"index":0,"embedding":[1]}]}`)
	if _, err := parseEmbedResponse(body, 2); err == nil {
		t.Fatal("expected error on count mismatch")
	}
}

func TestParseEmbedResponse_IndexOutOfRange(t *testing.T) {
	body := []byte(`{"data":[{"index":0,"embedding":[1]},{"index":5,"embedding":[2]}]}`)
	if _, err := parseEmbedResponse(body, 2); err == nil {
		t.Fatal("expected error on out-of-range index")
	}
}

func TestParseEmbedResponse_DuplicateIndex(t *testing.T) {
	body := []byte(`{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`)
	if _, err := parseEmbedResponse(body, 2); err == nil {
		t.Fatal("expected error on duplicate index")
	}
}
