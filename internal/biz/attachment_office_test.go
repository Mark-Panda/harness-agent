package biz

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildZip 构造测试用 zip 包，便于验证 docx/xlsx 抽取。
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buffer.Bytes()
}

// TestExtractDocxText 验证 Word 正文抽取保留段落结构。
func TestExtractDocxText(t *testing.T) {
	document := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:r><w:t>第一段</w:t></w:r></w:p>
<w:p><w:r><w:t>第二段</w:t><w:tab/><w:t>制表符后</w:t></w:r></w:p>
</w:body>
</w:document>`
	data := buildZip(t, map[string]string{"word/document.xml": document})
	text := extractDocxText(data)
	if !bytes.Contains([]byte(text), []byte("第一段")) || !bytes.Contains([]byte(text), []byte("第二段")) {
		t.Fatalf("docx text = %q", text)
	}
	if !bytes.Contains([]byte(text), []byte("\t")) {
		t.Fatalf("docx text lost tab: %q", text)
	}
}

// TestExtractXlsxText 验证 Excel 共享字符串与数值单元格抽取。
func TestExtractXlsxText(t *testing.T) {
	shared := `<?xml version="1.0" encoding="UTF-8"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<si><t>姓名</t></si><si><t>年龄</t></si><si><t>张三</t></si>
</sst>`
	sheet := `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>30</v></c></row>
</sheetData>
</worksheet>`
	data := buildZip(t, map[string]string{
		"xl/sharedStrings.xml":     shared,
		"xl/worksheets/sheet1.xml": sheet,
	})
	text := extractXlsxText(data)
	for _, want := range []string{"姓名", "年龄", "张三", "30"} {
		if !bytes.Contains([]byte(text), []byte(want)) {
			t.Fatalf("xlsx text missing %q: %q", want, text)
		}
	}
}

// TestExtractPDFText 验证未压缩内容流中的文本抽取。
func TestExtractPDFText(t *testing.T) {
	pdf := "%PDF-1.4\n1 0 obj\n<< /Length 44 >>\nstream\nBT /F1 12 Tf (Hello PDF) Tj ET\nendstream\nendobj\n%%EOF"
	text := extractPDFText([]byte(pdf))
	if !bytes.Contains([]byte(text), []byte("Hello PDF")) {
		t.Fatalf("pdf text = %q", text)
	}
}

// TestExtractPDFHexAndToUnicodeText 验证 UTF-16 hex 与 CID/ToUnicode 中文文本层不会被误判为扫描件。
func TestExtractPDFHexAndToUnicodeText(t *testing.T) {
	utf16PDF := "%PDF-1.4\n1 0 obj\n<< /Length 44 >>\nstream\nBT /F1 12 Tf <FEFF4E2D6587> Tj ET\nendstream\nendobj\n%%EOF"
	if text := extractPDFText([]byte(utf16PDF)); !strings.Contains(text, "中文") {
		t.Fatalf("UTF-16 PDF text = %q", text)
	}
	cidPDF := `%PDF-1.4
1 0 obj
<< /ToUnicode 2 0 R >>
endobj
2 0 obj
<< /Length 128 >>
stream
/CIDInit /ProcSet findresource begin
beginbfrange
<0001> <0002> [<4E2D> <6587>]
endbfrange
endstream
endobj
3 0 obj
<< /Resources << /Font << /F1 1 0 R >> >> >>
endobj
4 0 obj
<< /Length 64 >>
stream
BT /F1 12 Tf [<0001> <0002>] TJ ET
endstream
endobj
5 0 obj
<< /Length 64 >>
stream
BT /F2 12 Tf <41> Tj ET
endstream
endobj
%%EOF`
	text := extractPDFText([]byte(cidPDF))
	if !strings.Contains(text, "中") || !strings.Contains(text, "文") {
		t.Fatalf("CID PDF text = %q", text)
	}
	// F2 没有 ToUnicode，不能错误套用 F1 的 0001->中 映射。
	if !strings.Contains(text, "A") || strings.Contains(text, "�") {
		t.Fatalf("unmapped font used another font CMap: %q", text)
	}
}

// TestExtractAttachmentTextDispatch 验证二进制扩展名走结构化抽取，而不是被当作 UTF-8 文本。
func TestExtractAttachmentTextDispatch(t *testing.T) {
	document := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body><w:p><w:r><w:t>附件正文</w:t></w:r></w:p></w:body></w:document>`
	text, truncated := extractAttachmentText("report.docx", "", buildZip(t, map[string]string{"word/document.xml": document}))
	if truncated || text == "" || !bytes.Contains([]byte(text), []byte("附件正文")) {
		t.Fatalf("docx attachment text = %q, truncated = %v", text, truncated)
	}
	// 扫描件没有文本层时返回明确说明，避免模型误以为用户没有上传文件。
	if text, _ := extractAttachmentText("scan.pdf", "application/pdf", []byte("%PDF-1.4\n%%EOF")); !strings.Contains(text, "扫描件") {
		t.Fatalf("pdf without text should produce a diagnostic note: %q", text)
	}
}
