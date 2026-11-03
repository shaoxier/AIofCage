package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// TerminalAttachment 是写入沙箱的一个附件。Extension 非空时是宿主已校验的图片，保存为
// <序号>.<扩展名>；否则是普通文件，Name 必须是 AttachmentName 清理过的名称，保存为 <序号>-<名称>。
type TerminalAttachment struct {
	Data      []byte
	Extension string
	Name      string
}

// attachmentRecord 是交给沙箱内脚本的一项：完整的文件名由宿主决定，脚本只复核。
type attachmentRecord struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

const (
	maxAttachments          = 4
	maxAttachmentBytes      = 10 << 20
	maxAttachmentTotalBytes = 20 << 20
	// 名称按 UTF-8 字节计；加上序号前缀后仍远低于 Linux 单个文件名 255 字节的上限。
	maxAttachmentNameBytes = 120
	// 更长的后缀不当作扩展名，截断时不单独保留。
	maxAttachmentExtensionBytes = 16
	// defaultAttachmentName 用于清理后为空的名称，例如 ".."、只有空白或只有目录的路径。
	defaultAttachmentName = "附件"
	attachmentDirectory   = "/home/agent/.aiofcage/attachments/"
	// attachmentNameReplaced 是路径与 shell 中有特殊含义的 ASCII 字符：Agent 常把附件路径直接放进命令行，
	// 替换后不会展开变量或执行命令。空格、方括号、花括号、~ 和 # 仍保留，没加引号时会被 shell 拆开或展开，
	// 所以消息里的路径一律用单引号括起来；单引号本身在这里被替换，不会出现在名称里。
	attachmentNameReplaced = "/\\:*?\"<>|$`;&'!"
)

var attachmentBatchPattern = regexp.MustCompile(`^[A-Za-z0-9]{20,40}$`)

// AttachmentName 返回附件的显示名，它同时是沙箱里文件名的主体：只保留最后一个路径分量；路径分隔符、
// 控制字符（含 NUL、换行、制表符）、格式字符（例如双向文本控制符）、无效编码和 shell 特殊字符换成下划线，
// 其他空白（例如全角空格）换成普通空格并去掉首尾空白；"." 与 ".." 这类名称改用默认名；过长时截断并尽量
// 保留扩展名。对结果再次清理不会改变它。
func AttachmentName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	// macOS 常把文件名存成分解形式（例如 é 是 e 加组合重音）；统一成组合形式，模型重打路径时才对得上。
	raw = norm.NFC.String(raw)
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r == utf8.RuneError || unicode.IsControl(r) || strings.ContainsRune(attachmentNameReplaced, r):
			b.WriteByte('_')
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.IsGraphic(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.TrimSpace(b.String())
	if len(name) > maxAttachmentNameBytes {
		stem, ext := name, ""
		if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= maxAttachmentExtensionBytes {
			stem, ext = name[:i], name[i:]
		}
		cut := maxAttachmentNameBytes - len(ext)
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		name = strings.TrimSpace(stem[:cut]) + ext
	}
	if strings.Trim(name, ". ") == "" {
		return defaultAttachmentName
	}
	return name
}

// attachmentRecords 校验本批附件，决定每个附件在批次目录中的文件名，并返回与 files 顺序一致的沙箱路径。
func attachmentRecords(batchID string, files []TerminalAttachment) ([]attachmentRecord, []string, error) {
	invalid := errors.New("附件参数无效")
	if !attachmentBatchPattern.MatchString(batchID) || len(files) == 0 || len(files) > maxAttachments {
		return nil, nil, invalid
	}
	records := make([]attachmentRecord, len(files))
	paths := make([]string, len(files))
	total := 0
	for i, file := range files {
		total += len(file.Data)
		if len(file.Data) == 0 || len(file.Data) > maxAttachmentBytes || total > maxAttachmentTotalBytes {
			return nil, nil, invalid
		}
		var name string
		switch {
		case file.Name == "" && imageExtension(file.Extension):
			name = fmt.Sprintf("%d.%s", i+1, file.Extension)
		case file.Extension == "" && file.Name != "" && AttachmentName(file.Name) == file.Name:
			name = fmt.Sprintf("%d-%s", i+1, file.Name)
		default:
			return nil, nil, invalid
		}
		records[i] = attachmentRecord{Name: name, Data: file.Data}
		paths[i] = attachmentDirectory + batchID + "/" + name
	}
	return records, paths, nil
}

func imageExtension(ext string) bool {
	switch ext {
	case "png", "jpg", "gif", "webp":
		return true
	}
	return false
}

// WriteTerminalImages 把本批附件（图片与普通文件）写进沙箱 home 下新建的批次目录，返回与 files
// 顺序一致的路径。内容经标准输入传递，不进入命令参数或日志；写入失败时删除已写入的部分。
func (m *Manager) WriteTerminalImages(ctx context.Context, id, batchID string, files []TerminalAttachment) ([]string, error) {
	records, paths, err := attachmentRecords(batchID, files)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	if err := m.attachmentScript(ctx, id, bytes.NewReader(data), "write", batchID); err != nil {
		// 也处理上传中断后只写入一部分的情况。
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = m.RemoveTerminalImages(cleanup, id, batchID)
		for _, file := range files {
			if file.Extension == "" {
				return nil, errors.New("附件未能保存到沙箱，请确认沙箱正在运行后重试")
			}
		}
		return nil, errors.New("图片未能保存到沙箱，请确认沙箱正在运行后重试")
	}
	return paths, nil
}

// RemoveTerminalImages 删除一个批次的附件；批次目录不存在时视为成功。
func (m *Manager) RemoveTerminalImages(ctx context.Context, id, batchID string) error {
	if !attachmentBatchPattern.MatchString(batchID) {
		return errors.New("附件参数无效")
	}
	return m.attachmentScript(ctx, id, nil, "remove", batchID)
}

// attachmentScript 以 Agent 用户直接运行附件脚本（ExecHelper、PythonHelper），不加载 Agent 可写的
// ~/.aiofcage/env，工作目录、PYTHON* 变量和用户 site-packages 都不参与模块查找。脚本的输出不回到宿主，只看退出码。
func (m *Manager) attachmentScript(ctx context.Context, id string, stdin io.Reader, operation, batchID string) error {
	script, err := assets.ReadFile("assets/agent/terminal-images.py")
	if err != nil {
		return err
	}
	return m.ExecHelper(ctx, id, stdin, io.Discard, io.Discard, PythonHelper(string(script), operation, batchID)...)
}
