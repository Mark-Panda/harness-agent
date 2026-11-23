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

// TestRankingMetrics 验证 MRR 与 nDCG 对排序位置的敏感度。
func TestRankingMetrics(t *testing.T) {
	relevant := map[string]struct{}{"a": {}, "b": {}}
	if mrr := MRR([]string{"x", "a", "b"}, relevant); math.Abs(mrr-0.5) > 1e-9 {
		t.Fatalf("mrr = %f", mrr)
	}
	if mrr := MRR([]string{"x", "y"}, relevant); mrr != 0 {
		t.Fatalf("miss mrr = %f", mrr)
	}
	if ndcg := NDCGAtK([]string{"a", "b"}, relevant, 2); math.Abs(ndcg-1) > 1e-9 {
		t.Fatalf("perfect ndcg = %f", ndcg)
	}
	if ndcg := NDCGAtK([]string{"x", "a", "b"}, relevant, 3); ndcg >= 1 || ndcg <= 0 {
		t.Fatalf("downgraded ndcg = %f", ndcg)
	}
	if ndcg := NDCGAtK(nil, relevant, 2); ndcg != 0 {
		t.Fatalf("empty ndcg = %f", ndcg)
	}
}

// TestClaimMetrics 验证 claim 级摘要召回与禁止 claim 检测。
func TestClaimMetrics(t *testing.T) {
	summary := "用户偏好中文回答；部署窗口是周末。"
	if recall := SummaryClaimRecall(summary, []string{"偏好中文回答", "部署窗口是周末"}); recall != 1 {
		t.Fatalf("claim recall = %f", recall)
	}
	if recall := SummaryClaimRecall(summary, []string{"预算 9999 元"}); recall != 0 {
		t.Fatalf("missing claim recall = %f", recall)
	}
	if rate := UnsupportedClaimRate(summary, []string{"预算 9999 元", "部署窗口是周三"}); rate != 0 {
		t.Fatalf("unsupported rate = %f", rate)
	}
	if rate := UnsupportedClaimRate(summary, []string{"部署窗口是周末"}); rate != 1 {
		t.Fatalf("matched forbidden rate = %f", rate)
	}
}

// TestSpeakerAttributionAccuracy 验证 claim 是否归到正确发言人。
func TestSpeakerAttributionAccuracy(t *testing.T) {
	transcript := "ou_a：部署窗口是周六\nou_b：部署窗口是周日"
	expected := map[string]string{"ou_a": "部署窗口是周六", "ou_b": "部署窗口是周日"}
	if accuracy := SpeakerAttributionAccuracy(expected, transcript); accuracy != 1 {
		t.Fatalf("attribution accuracy = %f", accuracy)
	}
	wrong := map[string]string{"ou_a": "部署窗口是周日", "ou_b": "部署窗口是周六"}
	if accuracy := SpeakerAttributionAccuracy(wrong, transcript); accuracy != 0 {
		t.Fatalf("wrong attribution accuracy = %f", accuracy)
	}
}

// TestConflictResolutionAccuracy 验证生效事实与期望值的一致性。
func TestConflictResolutionAccuracy(t *testing.T) {
	active := map[string]string{"preference.language": "zh"}
	if accuracy := ConflictResolutionAccuracy(active, map[string]string{"preference.language": "zh"}); accuracy != 1 {
		t.Fatalf("conflict accuracy = %f", accuracy)
	}
	if accuracy := ConflictResolutionAccuracy(active, map[string]string{"preference.language": "en"}); accuracy != 0 {
		t.Fatalf("stale conflict accuracy = %f", accuracy)
	}
}

// TestMultilingualFaithfulness 验证日语假名与韩文不会被 tokenizer 丢弃。
func TestMultilingualFaithfulness(t *testing.T) {
	sources := []string{"こんにちは、今日はいい天気です。", "안녕하세요 오늘 날씨가 좋습니다."}
	if score := SummaryFaithfulness("こんにちは、今日はいい天気です。", sources[:1]); score < 0.8 {
		t.Fatalf("japanese faithfulness = %f", score)
	}
	if score := SummaryFaithfulness("안녕하세요 오늘 날씨가 좋습니다.", sources[1:]); score < 0.8 {
		t.Fatalf("korean faithfulness = %f", score)
	}
	hallucinated := SummaryFaithfulness("さようなら、明日は雨です。", sources[:1])
	if hallucinated >= 0.5 {
		t.Fatalf("japanese hallucination faithfulness = %f", hallucinated)
	}
}
