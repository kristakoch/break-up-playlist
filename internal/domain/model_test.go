package domain

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestDecisionValidation(t *testing.T) {
	cats := []Category{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	tests := []struct {
		name string
		p    Prediction
		ok   bool
	}{{"highest", Prediction{Category: "a", Probabilities: map[string]float64{"a": .72, "b": .2, "c": .08}}, true}, {"wrong winner", Prediction{Category: "b", Probabilities: map[string]float64{"a": .72, "b": .2, "c": .08}}, false}, {"missing", Prediction{Category: "a", Probabilities: map[string]float64{"a": 1}}, false}, {"nonfinite", Prediction{Category: "a", Probabilities: map[string]float64{"a": math.NaN(), "b": 0, "c": 0}}, false}, {"sum", Prediction{Category: "a", Probabilities: map[string]float64{"a": .9, "b": .9, "c": .9}}, false}, {"tie", Prediction{Category: "b", Probabilities: map[string]float64{"a": .5, "b": .5, "c": 0}}, true}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidatePrediction(tt.p, cats) == nil; got != tt.ok {
				t.Fatalf("valid=%v, want %v", got, tt.ok)
			}
		})
	}
}

func TestLowScoreRemainsAssigned(t *testing.T) {
	cats := []Category{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	p := Prediction{Category: "a", Probabilities: map[string]float64{"a": .28, "b": .25, "c": .24, "d": .23}}
	if err := ValidatePrediction(p, cats); err != nil {
		t.Fatal(err)
	}

	s := Song{Prediction: &p}

	if s.Assigned() != "a" || s.Score() >= .3 {
		t.Fatal("low score assignment was lost")
	}

	s.Manual = "b"

	if s.Assigned() != "b" {
		t.Fatal("manual choice must win")
	}
}

func TestEvidenceGateAndPrivacy(t *testing.T) {
	s := Song{Metadata: Metadata{ID: "1", Title: "Title"}, Manual: "secret-manual", Reference: "secret-reference", Lyrics: "  "}

	if s.Eligible() {
		t.Fatal("metadata and whitespace are insufficient")
	}

	s.Description = "Evidence"

	if !s.Eligible() {
		t.Fatal("description should qualify")
	}

	b, _ := json.Marshal(ChoiceRequest{Evidence: s.Evidence()})

	if strings.Contains(string(b), "secret") {
		t.Fatal("human choices leaked")
	}
}

func TestEvaluationIgnoresManual(t *testing.T) {
	c := Collection{Categories: []Category{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}, Songs: []Song{{Reference: "a", Manual: "b", Prediction: &Prediction{Category: "a"}}, {Reference: "b", Manual: "b", Prediction: &Prediction{Category: "a"}}, {Reference: "b"}}}
	e := Evaluate(c)

	if e.Correct != 1 || e.Evaluated != 2 || e.Labeled != 3 || e.Accuracy != .5 || math.Abs(e.MacroF1-1.0/3) > 1e-9 {
		t.Fatalf("unexpected evaluation: %+v", e)
	}

	c.Songs[0].Manual = "a"

	if Evaluate(c).Accuracy != e.Accuracy {
		t.Fatal("manual edit changed metrics")
	}
}

func TestFingerprintInputs(t *testing.T) {
	s := Song{Description: "Evidence"}
	cats := []Category{{ID: "a", Name: "A", Description: "one"}, {ID: "b", Name: "B", Description: "two"}}
	base := Fingerprint(s, cats, "m1")
	s.Manual = "a"
	s.Reference = "b"

	if Fingerprint(s, cats, "m1") != base {
		t.Fatal("human choices must not affect cache")
	}

	s.Description = "New"

	if Fingerprint(s, cats, "m1") == base {
		t.Fatal("evidence must affect cache")
	}

	s.Description = "Evidence"

	if Fingerprint(s, cats, "m2") == base {
		t.Fatal("model must affect cache")
	}

	cats[0], cats[1] = cats[1], cats[0]

	if Fingerprint(s, cats, "m1") == base {
		t.Fatal("category order must affect cache")
	}
}
