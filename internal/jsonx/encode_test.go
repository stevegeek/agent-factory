package jsonx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func loadGolden(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "py", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

type encodeGolden struct {
	Floats []struct {
		Hex  string `json:"hex"`
		JSON string `json:"json"`
	} `json:"floats"`
	Ints []struct {
		Text string `json:"text"`
		JSON string `json:"json"`
	} `json:"ints"`
	Strings []struct {
		CPs  []int32 `json:"cps"`
		JSON string  `json:"json"`
	} `json:"strings"`
	Structures []struct {
		In               string `json:"in"`
		Indent2          string `json:"indent2"`
		Default          string `json:"default"`
		Canonical        string `json:"canonical"`
		NumbersReencoded bool   `json:"numbers_reencoded"`
	} `json:"structures"`
}

func encodeCorpus(t *testing.T) encodeGolden {
	var g encodeGolden
	loadGolden(t, "encode.json", &g)
	if len(g.Floats) == 0 || len(g.Ints) == 0 || len(g.Strings) == 0 || len(g.Structures) == 0 {
		t.Fatal("encode.json is missing a section")
	}
	return g
}

// pyHex reads Python's float.hex() output.
func pyHex(t *testing.T, s string) float64 {
	switch s {
	case "inf":
		return math.Inf(1)
	case "-inf":
		return math.Inf(-1)
	case "nan":
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("float.hex %q: %v", s, err)
	}
	return f
}

// wtf8 builds a Go string from code points the way jsonx holds them: lone
// surrogates as their generalized UTF-8 bytes (WTF-8).
func wtf8(cps []int32) string {
	var b strings.Builder
	for _, r := range cps {
		if r >= 0xD800 && r <= 0xDFFF {
			b.WriteByte(byte(0xE0 | r>>12))
			b.WriteByte(byte(0x80 | (r>>6)&0x3F))
			b.WriteByte(byte(0x80 | r&0x3F))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestFloatsEncodeAsPythonRepr(t *testing.T) {
	for _, c := range encodeCorpus(t).Floats {
		f := pyHex(t, c.Hex)
		if got := string(EncodePython(Float(f), -1)); got != c.JSON {
			t.Errorf("Float(%v) (%s) encodes as %s, Python %s", f, c.Hex, got, c.JSON)
		}
		// Reading Python's output back gives the same text.
		v, err := Parse([]byte(c.JSON))
		if err != nil {
			t.Errorf("Parse(%s): %v", c.JSON, err)
			continue
		}
		if got := string(EncodePython(v, -1)); got != c.JSON {
			t.Errorf("Parse(%s) re-encodes as %s", c.JSON, got)
		}
		if g, ok := v.Float64(); !ok || (g != f && !(math.IsNaN(f) && math.IsNaN(g))) || math.Signbit(g) != math.Signbit(f) {
			t.Errorf("Parse(%s).Float64() = %v, %v; want %v", c.JSON, g, ok, f)
		}
	}
}

func TestIntsEncodeAsPython(t *testing.T) {
	for _, c := range encodeCorpus(t).Ints {
		v, err := Number(c.Text)
		if err != nil {
			t.Fatalf("Number(%s): %v", c.Text, err)
		}
		if got := string(EncodePython(v, -1)); got != c.JSON {
			t.Errorf("Number(%s) encodes as %s, Python %s", c.Text, got, c.JSON)
		}
		if i, err := strconv.ParseInt(c.Text, 10, 64); err == nil {
			if got := string(EncodePython(Int(i), -1)); got != c.JSON {
				t.Errorf("Int(%d) encodes as %s, Python %s", i, got, c.JSON)
			}
			if g, ok := v.Int64(); !ok || g != i {
				t.Errorf("Number(%s).Int64() = %d, %v", c.Text, g, ok)
			}
		} else if _, ok := v.Int64(); ok {
			t.Errorf("Number(%s).Int64() claims to fit", c.Text)
		}
	}
}

func TestStringsEncodeAsPythonEnsureASCII(t *testing.T) {
	for _, c := range encodeCorpus(t).Strings {
		s := wtf8(c.CPs)
		if got := string(EncodePython(String(s), -1)); got != c.JSON {
			t.Errorf("String(%v) encodes as %s, Python %s", c.CPs, got, c.JSON)
		}
		v, err := Parse([]byte(c.JSON))
		if err != nil {
			t.Errorf("Parse(%s): %v", c.JSON, err)
			continue
		}
		if got, ok := v.Str(); !ok || got != s {
			t.Errorf("Parse(%s) = %q, want %q", c.JSON, got, s)
		}
	}
}

func TestStructuresEncodeAsPython(t *testing.T) {
	for _, c := range encodeCorpus(t).Structures {
		v, err := Parse([]byte(c.In))
		if err != nil {
			t.Errorf("Parse(%s): %v", c.In, err)
			continue
		}
		if got := string(EncodePython(v, 2)); got != c.Indent2 {
			t.Errorf("%s with indent=2:\n%s\nPython:\n%s", c.In, got, c.Indent2)
		}
		if got := string(EncodePython(v, -1)); got != c.Default {
			t.Errorf("%s by default: %s, Python %s", c.In, got, c.Default)
		}
		if got := string(Canonical(v)); got != c.Canonical {
			t.Errorf("%s canonical: %s, Python %s", c.In, got, c.Canonical)
		}
		// Python's own output is a fixed point.
		w, err := Parse([]byte(c.Indent2))
		if err != nil || string(EncodePython(w, 2)) != c.Indent2 {
			t.Errorf("Python's indent=2 output of %s does not round-trip byte for byte (%v)", c.In, err)
		}
	}
}

func TestDigestMatchesRequestDigest(t *testing.T) {
	var g struct {
		Rows []struct {
			In        string `json:"in"`
			Canonical string `json:"canonical"`
			Digest    string `json:"digest"`
		} `json:"rows"`
	}
	loadGolden(t, "digest.json", &g)
	if len(g.Rows) == 0 {
		t.Fatal("no digest rows")
	}
	for _, r := range g.Rows {
		v, err := Parse([]byte(r.In))
		if err != nil {
			t.Fatal(err)
		}
		o, _ := v.Object()
		o.Delete("idempotency_key")
		c := Canonical(o.Value())
		if string(c) != r.Canonical {
			t.Errorf("canonical %s, Python %s", c, r.Canonical)
		}
		sum := sha256.Sum256(c)
		if got := hex.EncodeToString(sum[:]); got != r.Digest {
			t.Errorf("digest of %s: %s, Python %s", r.In, got, r.Digest)
		}
	}
}

func TestFormats(t *testing.T) {
	v, err := Parse([]byte(`{"b": [1, {"d": null, "c": true}], "a": {}, "é": []}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		f    Format
		want string
	}{
		{Format{Indent: -1}, `{"b": [1, {"d": null, "c": true}], "a": {}, "\u00e9": []}`},
		{Format{Indent: -1, Compact: true}, `{"b":[1,{"d":null,"c":true}],"a":{},"\u00e9":[]}`},
		{Format{Indent: 0}, "{\n\"b\": [\n1,\n{\n\"d\": null,\n\"c\": true\n}\n],\n\"a\": {},\n\"\\u00e9\": []\n}"},
		{Format{Indent: 2, SortKeys: true}, "{\n  \"a\": {},\n  \"b\": [\n    1,\n    {\n      \"c\": true,\n      \"d\": null\n    }\n  ],\n  \"\\u00e9\": []\n}"},
	} {
		if got := string(c.f.Encode(v)); got != c.want {
			t.Errorf("%+v:\n%s\nwant\n%s", c.f, got, c.want)
		}
	}
}
