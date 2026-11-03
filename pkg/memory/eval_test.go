package memory

import (
	"math"
	"testing"
)

// TestPrecisionRecall 验证 Top-K 指标与空相关集合语义。
func TestPrecisionRecall(t *testing.T) {
	ranked := []string{"a", "b", "c", "d"}
	relevant := map[string]struct{}{"a": {}, "d": {}}
	if precision := PrecisionAtK(ranked, relevant, 2); math.Abs(precision-0.5) > 1e-9 {
		t.Fatalf("precision = %f", precision)
	}
	if recall := RecallAtK(ranked, relevant, 4); math.Abs(recall-1) > 1e-9 {
		t.Fatalf("recall = %f", recall)
	}
	if recall := RecallAtK(ranked, relevant, 1); math.Abs(recall-0.5) > 1e-9 {
		t.Fatalf("partial recall = %f", recall)
	}
	if recall := RecallAtK(ranked, nil, 4); recall != 1 {
		t.Fatalf("empty relevant recall = %f", recall)
	}
	if precision := PrecisionAtK(nil, relevant, 2); precision != 0 {
		t.Fatalf("empty ranked precision = %f", precision)
	}
	if precision := PrecisionAtK(ranked, relevant, 0); precision != 0 {
		t.Fatalf("zero k precision = %f", precision)
	}
	// 重复结果只计一次召回。
	if recall := RecallAtK([]string{"a", "a", "b"}, map[string]struct{}{"a": {}}, 3); recall != 1 {
		t.Fatalf("duplicate recall = %f", recall)
	}
}

// TestMMRDiversity 验证相同向量多样性为 0、正交向量多样性为 1。
func TestMMRDiversity(t *testing.T) {
	if diversity := MMRDiversity([][]float32{{1, 0}, {1, 0}}); math.Abs(diversity) > 1e-6 {
		t.Fatalf("identical diversity = %f", diversity)
	}
	if diversity := MMRDiversity([][]float32{{1, 0}, {0, 1}}); math.Abs(diversity-1) > 1e-6 {
		t.Fatalf("orthogonal diversity = %f", diversity)
	}
	if diversity := MMRDiversity([][]float32{{1, 0}}); diversity != 0 {
		t.Fatalf("single diversity = %f", diversity)
	}
}

// TestSummaryFaithfulness 验证摘要忠实度随未来源内容增加而下降。
func TestSummaryFaithfulness(t *testing.T) {
	sources := []string{"用户偏好使用中文回答，部署窗口是周末进行发布。", "The user prefers Chinese responses."}
	faithful := SummaryFaithfulness("用户偏好中文回答，部署窗口周末发布。", sources)
	if faithful < 0.7 {
		t.Fatalf("faithful summary = %f", faithful)
	}
	hallucinated := SummaryFaithfulness("用户偏好法语回答，部署窗口改到周三，预算为 9999 元。", sources)
	if hallucinated >= faithful {
		t.Fatalf("hallucinated = %f faithful = %f", hallucinated, faithful)
	}
	if faithfulness := SummaryFaithfulness("", sources); faithfulness != 1 {
		t.Fatalf("empty summary faithfulness = %f", faithfulness)
	}
}

// TestSpeakerAttributionRate 验证群聊发言人归属评测。
func TestSpeakerAttributionRate(t *testing.T) {
	transcript := FormatSummaryTranscript([]SummaryMessage{
		{Role: "user", Speaker: "ou_a", Content: "同意"},
		{Role: "user", Speaker: "ou_b", Content: "反对"},
	}, 0)
	if rate := SpeakerAttributionRate(transcript, []string{"ou_a", "ou_b"}); rate != 1 {
		t.Fatalf("attribution rate = %f", rate)
	}
	if rate := SpeakerAttributionRate("无发言人摘要", []string{"ou_a"}); rate != 0 {
		t.Fatalf("missing attribution rate = %f", rate)
	}
	if rate := SpeakerAttributionRate(transcript, nil); rate != 1 {
		t.Fatalf("empty speakers rate = %f", rate)
	}
}
