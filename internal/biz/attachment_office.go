package biz

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// extractDocxText 抽取 Word（.docx）正文文本。
// docx 是 zip 包，正文位于 word/document.xml，逐个读取 <w:t> 文本节点并在段落结束时换行。
func extractDocxText(data []byte) string {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		content, err := readZipFile(file)
		if err != nil {
			return ""
		}
		return docxDocumentText(content)
	}
	return ""
}

// docxDocumentText 解析 document.xml，保留段落换行与制表符。
func docxDocumentText(content []byte) string {
	text := newBoundedText(MaxExtractedChars + 1)
	decoder := xml.NewDecoder(bytes.NewReader(content))
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return text.String()
		}
		switch item := token.(type) {
		case xml.StartElement:
			switch item.Name.Local {
			case "t":
				depth++
			case "tab":
				text.write("\t")
			case "br":
				text.write("\n")
			}
		case xml.EndElement:
			switch item.Name.Local {
			case "t":
				if depth > 0 {
					depth--
				}
			case "p":
				text.write("\n")
			}
		case xml.CharData:
			if depth > 0 {
				text.write(string(item))
			}
		}
	}
	return text.String()
}

// extractXlsxText 抽取 Excel（.xlsx）单元格文本。
// 按工作表输出，共享字符串通过 xl/sharedStrings.xml 还原，行内字符串与数值直接读取。
func extractXlsxText(data []byte) string {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	files := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		files[file.Name] = file
	}
	shared := make([]string, 0)
	if file, ok := files["xl/sharedStrings.xml"]; ok {
		if content, err := readZipFile(file); err == nil {
			shared = parseSharedStrings(content)
		}
	}
	sheets := make([]string, 0, 4)
	for name := range files {
		if strings.HasPrefix(name, "xl/worksheets/sheet") && strings.HasSuffix(name, ".xml") {
			sheets = append(sheets, name)
		}
	}
	sort.Strings(sheets)
	text := newBoundedText(MaxExtractedChars + 1)
	for _, name := range sheets {
		content, err := readZipFile(files[name])
		if err != nil {
			continue
		}
		text.write("【工作表：" + filepath.Base(name) + "】\n")
		text.write(sheetText(content, shared))
		text.write("\n")
		if text.Full() {
			break
		}
	}
	return text.String()
}

// parseSharedStrings 解析共享字符串表，每个 <si> 聚合其下全部 <t> 文本。
func parseSharedStrings(content []byte) []string {
	decoder := xml.NewDecoder(bytes.NewReader(content))
	result := make([]string, 0, 64)
	var current strings.Builder
	inItem, inText := false, false
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch item := token.(type) {
		case xml.StartElement:
			switch item.Name.Local {
			case "si":
				inItem = true
				current.Reset()
			case "t":
				if inItem {
					inText = true
				}
			}
		case xml.EndElement:
			switch item.Name.Local {
			case "si":
				if inItem {
					result = append(result, current.String())
					inItem = false
				}
			case "t":
				inText = false
			}
		case xml.CharData:
			if inText {
				current.Write(item)
			}
		}
	}
	return result
}

// sheetText 解析单个工作表，按行输出制表符分隔的单元格文本。
func sheetText(content []byte, shared []string) string {
	text := newBoundedText(MaxExtractedChars + 1)
	decoder := xml.NewDecoder(bytes.NewReader(content))
	var value strings.Builder
	inCell, inValue, inInline := false, false, false
	cellType := ""
	firstRow := true
	firstCellInRow := true
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch item := token.(type) {
		case xml.StartElement:
			switch item.Name.Local {
			case "row":
				if !firstRow {
					text.write("\n")
				}
				firstRow = false
				firstCellInRow = true
			case "c":
				inCell = true
				value.Reset()
				cellType = attribute(item, "t")
				if !firstCellInRow {
					text.write("\t")
				}
				firstCellInRow = false
			case "v":
				if inCell {
					inValue = true
				}
			case "is":
				if inCell {
					inInline = true
				}
			}
		case xml.EndElement:
			switch item.Name.Local {
			case "c":
				if inCell {
					text.write(cellValue(cellType, value.String(), shared))
					inCell = false
				}
			case "v":
				inValue = false
			case "is":
				inInline = false
			}
		case xml.CharData:
			if inValue || inInline {
				value.Write(item)
			}
		}
		if text.Full() {
			break
		}
	}
	return text.String()
}

// cellValue 把单元格原始值转换为可读文本。
func cellValue(cellType, raw string, shared []string) string {
	switch cellType {
	case "s":
		if index, err := parseCellIndex(raw); err == nil && index >= 0 && index < len(shared) {
			return shared[index]
		}
		return raw
	case "inlineStr", "str":
		return raw
	default:
		return raw
	}
}

// parseCellIndex 解析共享字符串下标。
func parseCellIndex(raw string) (int, error) {
	var index int
	_, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &index)
	return index, err
}

// attribute 读取 XML 属性值，不存在时返回空串。
func attribute(element xml.StartElement, name string) string {
	for _, attr := range element.Attr {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

// extractPDFText 尽力抽取 PDF 文本：解压 FlateDecode 流并提取 Tj/TJ 文本操作符。
// 仅覆盖未加密、使用标准编码的常见数字 PDF；无法解码时返回空串，由上层按“未抽取到文本”处理。
func extractPDFText(data []byte) string {
	text := newBoundedText(MaxExtractedChars + 1)
	fonts := parsePDFFontCMaps(data)
	for _, stream := range pdfStreams(data) {
		content := stream
		if decoded, ok := inflate(stream); ok {
			content = decoded
		}
		// 只处理包含文本绘制操作符的内容流，避免把图片、字体等二进制流误当作文本。
		if !bytes.Contains(content, []byte("Tj")) && !bytes.Contains(content, []byte("TJ")) {
			continue
		}
		extractPDFTextOperators(content, text, fonts)
		if text.Full() {
			break
		}
	}
	return text.String()
}

// pdfStreams 切分 PDF 中的 stream...endstream 数据块。
func pdfStreams(data []byte) [][]byte {
	streams := make([][]byte, 0, 8)
	offset := 0
	for {
		start := bytes.Index(data[offset:], []byte("stream"))
		if start < 0 {
			break
		}
		start += offset + len("stream")
		// stream 关键字后必须跟随 CRLF 或 LF。
		if start < len(data) && data[start] == '\r' {
			start++
		}
		if start < len(data) && data[start] == '\n' {
			start++
		}
		end := bytes.Index(data[start:], []byte("endstream"))
		if end < 0 {
			break
		}
		streams = append(streams, data[start:start+end])
		offset = start + end + len("endstream")
	}
	return streams
}

// inflate 尝试以 zlib 解压数据，失败时返回 false 由调用方按明文处理。
func inflate(data []byte) ([]byte, bool) {
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, 8<<20))
	if err != nil || len(decoded) == 0 {
		return nil, false
	}
	return decoded, true
}

var pdfFontSwitchPattern = regexp.MustCompile(`^/([F][A-Za-z0-9_.-]+)\s+[-+0-9.]+\s+Tf`)

// extractPDFTextOperators 从内容流中提取文本字符串，支持字面量、十六进制 Tj/TJ 与字体 ToUnicode 映射。
func extractPDFTextOperators(content []byte, text *boundedText, fonts pdfFontCMaps) {
	var currentFont *pdfCMap
	fontExplicit := false
	for index := 0; index < len(content) && !text.Full(); {
		if content[index] == '/' {
			if match := pdfFontSwitchPattern.FindSubmatch(content[index:]); match != nil {
				currentFont = fonts.byName[string(match[1])]
				fontExplicit = true
				index += len(match[0])
				continue
			}
		}
		switch content[index] {
		case '(':
			value, next := readPDFString(content, index)
			text.write(value)
			text.write(" ")
			index = next
		case '<':
			// <<...>> 是字典而非文本；单个 <...> 则可能是 UTF-16 或 CID 字符串。
			if index+1 < len(content) && content[index+1] == '<' {
				if next := bytes.Index(content[index+2:], []byte(">>")); next >= 0 {
					index += next + 4
					continue
				}
				return
			}
			next := bytes.IndexByte(content[index:], '>')
			if next < 0 {
				return
			}
			raw, err := decodePDFHexBytes(content[index+1 : index+next])
			if err == nil && len(raw) > 0 {
				text.write(decodePDFHexText(raw, currentFont, fontExplicit, fonts))
				text.write(" ")
			}
			index += next + 1
		default:
			index++
		}
	}
}

// decodePDFHexBytes 去除 PDF 十六进制字符串中的空白并解码。
func decodePDFHexBytes(data []byte) ([]byte, error) {
	cleaned := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(string(data))
	return hex.DecodeString(cleaned)
}

// decodePDFHexText 优先使用当前字体的 ToUnicode；无映射时兼容常见 UTF-16BE 与 Latin-1。
// 只有未明确选择字体时才允许使用文档唯一 CMap，已切换到其他字体时不得串用映射。
func decodePDFHexText(raw []byte, current *pdfCMap, fontExplicit bool, fonts pdfFontCMaps) string {
	if current == nil && !fontExplicit && len(fonts.all) == 1 {
		current = fonts.all[0]
	}
	if current != nil {
		return current.decode(raw)
	}
	if looksLikeUTF16BE(raw) {
		return decodeUTF16BE(raw)
	}
	return decodePDFString(raw)
}

// looksLikeUTF16BE 区分常见 UTF-16 文本与普通 ASCII hex，避免把 ASCII 两两分组误解码。
func looksLikeUTF16BE(data []byte) bool {
	if len(data) < 2 || len(data)%2 != 0 {
		return false
	}
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		return true
	}
	for _, value := range data {
		if value == 0 || value >= 0x80 {
			return true
		}
	}
	return false
}

// decodeUTF16BE 解码不带 BOM 的 UTF-16BE 字节序列。
func decodeUTF16BE(data []byte) string {
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		return decodePDFString(data)
	}
	words := make([]uint16, 0, len(data)/2)
	for index := 0; index+1 < len(data); index += 2 {
		words = append(words, binary.BigEndian.Uint16(data[index:index+2]))
	}
	return string(utf16.Decode(words))
}

// decode 按 CMap 的固定源码宽度把 CID 序列映射为 Unicode。
func (m *pdfCMap) decode(raw []byte) string {
	codeBytes := m.codeBytes
	if codeBytes <= 0 {
		codeBytes = 2
	}
	var builder strings.Builder
	for offset := 0; offset < len(raw); offset += codeBytes {
		end := offset + codeBytes
		if end > len(raw) {
			end = len(raw)
		}
		chunk := raw[offset:end]
		key := strings.ToUpper(hex.EncodeToString(chunk))
		if value, ok := m.entries[key]; ok {
			builder.WriteString(value)
			continue
		}
		if len(chunk) == 1 && chunk[0] < 0x80 {
			builder.WriteByte(chunk[0])
		} else {
			builder.WriteString("�")
		}
	}
	return builder.String()
}

// pdfCMap 保存一个字体 ToUnicode 表的源码宽度与映射关系。
type pdfCMap struct {
	entries   map[string]string
	codeBytes int
}

// pdfFontCMaps 保存资源名到字体映射及文档中的全部映射，单字体文档可直接回退。
type pdfFontCMaps struct {
	byName map[string]*pdfCMap
	all    []*pdfCMap
}

var (
	pdfObjectPattern       = regexp.MustCompile(`(?s)(\d+)\s+\d+\s+obj\b(.*?)\bendobj\b`)
	pdfToUnicodePattern    = regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`)
	pdfFontResourcePattern = regexp.MustCompile(`/([F][A-Za-z0-9_.-]+)\s+(\d+)\s+\d+\s+R`)
	pdfHexTokenPattern     = regexp.MustCompile(`<([0-9A-Fa-f]+)>`)
)

// parsePDFFontCMaps 从 PDF 对象中恢复字体资源名与 ToUnicode CMap 的关联。
func parsePDFFontCMaps(data []byte) pdfFontCMaps {
	fonts := pdfFontCMaps{byName: map[string]*pdfCMap{}}
	objects := pdfObjectPattern.FindAllSubmatch(data, -1)
	byObject := make(map[int]*pdfCMap)
	for _, match := range objects {
		objectID, err := strconv.Atoi(string(match[1]))
		if err != nil {
			continue
		}
		for _, stream := range pdfStreams(match[2]) {
			content := stream
			if decoded, ok := inflate(stream); ok {
				content = decoded
			}
			if !bytes.Contains(content, []byte("beginbf")) {
				continue
			}
			if cmap := parsePDFCMap(content); cmap != nil {
				byObject[objectID] = cmap
				fonts.all = append(fonts.all, cmap)
				break
			}
		}
	}
	fontObjects := make(map[int]*pdfCMap)
	for _, match := range objects {
		reference := pdfToUnicodePattern.FindSubmatch(match[2])
		if reference == nil {
			continue
		}
		objectID, _ := strconv.Atoi(string(match[1]))
		cmapID, err := strconv.Atoi(string(reference[1]))
		if err == nil && byObject[cmapID] != nil {
			fontObjects[objectID] = byObject[cmapID]
		}
	}
	for _, match := range objects {
		for _, resource := range pdfFontResourcePattern.FindAllSubmatch(match[2], -1) {
			fontObjectID, err := strconv.Atoi(string(resource[2]))
			if err != nil {
				continue
			}
			if cmap := fontObjects[fontObjectID]; cmap != nil {
				fonts.byName[string(resource[1])] = cmap
			}
		}
	}
	return fonts
}

// parsePDFCMap 解析常见 beginbfchar 与 beginbfrange 映射，目标按 UTF-16BE 解码。
func parsePDFCMap(content []byte) *pdfCMap {
	cmap := &pdfCMap{entries: map[string]string{}}
	mode := ""
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(strings.SplitN(rawLine, "%", 2)[0])
		switch {
		case strings.Contains(line, "beginbfchar"):
			mode = "char"
			continue
		case strings.Contains(line, "beginbfrange"):
			mode = "range"
			continue
		case strings.Contains(line, "endbfchar"), strings.Contains(line, "endbfrange"):
			mode = ""
			continue
		}
		if mode == "" {
			continue
		}
		tokens := pdfHexTokenPattern.FindAllString(line, -1)
		if mode == "char" {
			for index := 0; index+1 < len(tokens); index += 2 {
				cmap.put(tokens[index], tokens[index+1])
			}
			continue
		}
		if len(tokens) < 3 {
			continue
		}
		start, errStart := strconv.ParseUint(pdfHexTokenValue(tokens[0]), 16, 64)
		end, errEnd := strconv.ParseUint(pdfHexTokenValue(tokens[1]), 16, 64)
		if errStart != nil || errEnd != nil || end < start || end-start > 65_536 {
			continue
		}
		if strings.Contains(line, "[") {
			for offset, destination := range tokens[2:] {
				cmap.put(fmt.Sprintf("%0*X", len(pdfHexTokenValue(tokens[0])), start+uint64(offset)), destination)
			}
			continue
		}
		destination := pdfHexTokenValue(tokens[2])
		for offset := uint64(0); offset <= end-start; offset++ {
			value := start + offset
			source := fmt.Sprintf("%0*X", len(pdfHexTokenValue(tokens[0])), value)
			target := fmt.Sprintf("%0*X", len(destination), offset+mustParsePDFHex(destination))
			cmap.put(source, target)
		}
	}
	if len(cmap.entries) == 0 {
		return nil
	}
	return cmap
}

// put 写入一条源码到 Unicode 的映射，并从首条源码推断固定字节宽度。
func (m *pdfCMap) put(sourceToken, destinationToken string) {
	source := pdfHexTokenValue(sourceToken)
	destination, err := hex.DecodeString(pdfHexTokenValue(destinationToken))
	if source == "" || err != nil || len(destination) == 0 {
		return
	}
	if m.codeBytes == 0 {
		m.codeBytes = len(source) / 2
	}
	m.entries[source] = decodeUTF16BE(destination)
}

// pdfHexTokenValue 去掉十六进制 token 两侧的尖括号。
func pdfHexTokenValue(token string) string {
	return strings.TrimSuffix(strings.TrimPrefix(token, "<"), ">")
}

// mustParsePDFHex 解析 bfrange 的起始目标值；非法值返回 0，随后会被目标解码校验过滤。
func mustParsePDFHex(value string) uint64 {
	parsed, _ := strconv.ParseUint(value, 16, 64)
	return parsed
}

// readPDFString 读取字面量字符串，处理转义与括号嵌套，返回文本与下一位置。
func readPDFString(content []byte, start int) (string, int) {
	builder := make([]byte, 0, 64)
	depth := 0
	for index := start; index < len(content); index++ {
		switch content[index] {
		case '\\':
			if index+1 >= len(content) {
				return decodePDFString(builder), len(content)
			}
			index++
			switch content[index] {
			case 'n':
				builder = append(builder, '\n')
			case 'r':
				builder = append(builder, '\r')
			case 't':
				builder = append(builder, '\t')
			case 'b', 'f':
				// 退格与换页不产生可读文本。
			default:
				if content[index] >= '0' && content[index] <= '7' {
					value := 0
					for count := 0; count < 3 && index < len(content) && content[index] >= '0' && content[index] <= '7'; count++ {
						value = value*8 + int(content[index]-'0')
						index++
					}
					index--
					builder = append(builder, byte(value))
					continue
				}
				builder = append(builder, content[index])
			}
		case '(':
			depth++
			if depth > 1 {
				builder = append(builder, content[index])
			}
		case ')':
			depth--
			if depth == 0 {
				return decodePDFString(builder), index + 1
			}
			builder = append(builder, content[index])
		case '\r', '\n':
			builder = append(builder, ' ')
		default:
			builder = append(builder, content[index])
		}
	}
	return decodePDFString(builder), len(content)
}

// decodePDFString 把 PDF 字符串字节转换为 UTF-8；UTF-16BE BOM 优先，其余按 Latin-1 处理。
func decodePDFString(data []byte) string {
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		words := make([]uint16, 0, (len(data)-2)/2)
		for index := 2; index+1 < len(data); index += 2 {
			words = append(words, binary.BigEndian.Uint16(data[index:index+2]))
		}
		return string(utf16.Decode(words))
	}
	runes := make([]rune, 0, len(data))
	for _, value := range data {
		if value >= 0x20 || value == '\n' || value == '\t' {
			runes = append(runes, rune(value))
		}
	}
	return string(runes)
}

// readZipFile 读取 zip 内单个文件，限制解压后大小，避免压缩炸弹。
func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, 64<<20))
}

// boundedText 是带字符预算的文本缓冲，达到上限后停止写入并标记已满。
type boundedText struct {
	builder strings.Builder
	runes   int
	limit   int
}

// newBoundedText 构造带字符上限的缓冲区。
func newBoundedText(limit int) *boundedText {
	return &boundedText{limit: limit}
}

// write 追加文本，超出预算时截断到剩余额度。
func (b *boundedText) write(value string) {
	if b.limit <= 0 || b.runes >= b.limit {
		return
	}
	runes := []rune(value)
	if remaining := b.limit - b.runes; len(runes) > remaining {
		runes = runes[:remaining]
	}
	b.runes += len(runes)
	b.builder.WriteString(string(runes))
}

// String 返回已写入文本。
func (b *boundedText) String() string { return b.builder.String() }

// Full 报告缓冲区是否已达字符上限。
func (b *boundedText) Full() bool { return b.limit > 0 && b.runes >= b.limit }
