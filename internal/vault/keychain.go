package vault

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound    = errors.New("钥匙串中没有这个条目")
	servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Keychain stores generic passwords in the user's login keychain through /usr/bin/security.
// Values are passed on stdin (interactive mode), never as command-line arguments, so other
// processes cannot read them from the process list.
type Keychain struct {
	Account string
	binary  string
}

func NewKeychain() *Keychain {
	account := os.Getenv("USER")
	if account == "" {
		account = "aiofcage"
	}
	return &Keychain{Account: account, binary: "/usr/bin/security"}
}

// Values that are not plain tokens (passwords with spaces or quotes, OAuth credentials as JSON)
// are stored base64-encoded behind encodedPrefix; long ones are split across "<service>-partN"
// items, because `security -i` truncates command lines at about 4 KB. Plain API keys are stored
// as they are, so items created elsewhere (e.g. with `security add-generic-password`) keep working.
const (
	encodedPrefix = "aiofcage-b64:"
	partsPrefix   = "aiofcage-parts:"
	maxLine       = 3400 // leaves room for the command and a 120-character item name
	partSize      = 3400
	maxParts      = 8
	maxValue      = 16 << 10
)

func (k *Keychain) Read(ctx context.Context, service string) ([]byte, error) {
	value, err := k.readItem(ctx, service)
	if err != nil {
		return nil, err
	}
	if rest, ok := bytes.CutPrefix(value, []byte(partsPrefix)); ok {
		count, first, found := bytes.Cut(rest, []byte(":"))
		n, convErr := strconv.Atoi(string(count))
		if !found || convErr != nil || n < 2 || n > maxParts {
			return nil, fmt.Errorf("钥匙串条目 %s 内容无效", service)
		}
		joined := append([]byte(nil), first...)
		for i := 2; i <= n; i++ {
			part, err := k.readItem(ctx, partName(service, i))
			if err != nil {
				return nil, fmt.Errorf("钥匙串条目 %s 不完整", service)
			}
			joined = append(joined, part...)
		}
		value = joined
	}
	if encoded, ok := bytes.CutPrefix(value, []byte(encodedPrefix)); ok {
		decoded, err := base64.StdEncoding.DecodeString(string(encoded))
		if err != nil {
			return nil, fmt.Errorf("钥匙串条目 %s 内容无效", service)
		}
		return decoded, nil
	}
	return value, nil
}

func partName(service string, i int) string { return fmt.Sprintf("%s-part%d", service, i) }

func (k *Keychain) readItem(ctx context.Context, service string) ([]byte, error) {
	if !servicePattern.MatchString(service) {
		return nil, errors.New("钥匙串条目名称格式无效")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, k.binary, "find-generic-password", "-a", k.Account, "-s", service, "-w")
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return nil, ErrNotFound
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("读取钥匙串（%s）被中断：%w", service, ctx.Err())
		}
		return nil, fmt.Errorf("读取钥匙串失败（%s）", service)
	}
	value := bytes.TrimRight(stdout.Bytes(), "\n")
	if len(value) == 0 || bytes.ContainsAny(value, "\r\n") {
		return nil, fmt.Errorf("钥匙串条目 %s 内容无效", service)
	}
	return value, nil
}

// Put creates or replaces an item (see encodedPrefix for how values are laid out).
func (k *Keychain) Put(ctx context.Context, service string, value []byte) error {
	if !servicePattern.MatchString(service) || len(service) > 120 {
		return errors.New("钥匙串条目名称格式无效")
	}
	if len(value) == 0 || len(value) > maxValue {
		return errors.New("密钥不能为空，也不能超过 16 KB")
	}
	stored := string(value)
	if len(value) > maxLine || bytes.ContainsAny(value, " \t\r\n\"'\\") || bytes.HasPrefix(value, []byte("aiofcage-")) {
		stored = encodedPrefix + base64.StdEncoding.EncodeToString(value)
	}
	var parts []string
	for len(stored) > maxLine {
		cut := partSize
		if len(parts) == 0 {
			cut = partSize - len(partsPrefix) - 2
		}
		parts, stored = append(parts, stored[:cut]), stored[cut:]
	}
	parts = append(parts, stored)
	if len(parts) > 1 {
		parts[0] = fmt.Sprintf("%s%d:%s", partsPrefix, len(parts), parts[0])
	}
	previous := k.partCount(ctx, service)
	for i := len(parts); i >= 1; i-- { // the head item last, so it only points at parts already written
		name := service
		if i > 1 {
			name = partName(service, i)
		}
		if err := k.putItem(ctx, name, parts[i-1]); err != nil {
			return err
		}
	}
	for i := len(parts) + 1; i <= previous; i++ {
		_ = k.deleteItem(ctx, partName(service, i))
	}
	stored2, err := k.Read(ctx, service)
	if err != nil || !bytes.Equal(stored2, value) {
		return fmt.Errorf("写入钥匙串后校验失败（%s）", service)
	}
	return nil
}

// partCount returns how many items a stored value spans (1 when it is not split or missing).
func (k *Keychain) partCount(ctx context.Context, service string) int {
	value, err := k.readItem(ctx, service)
	if err != nil {
		return 1
	}
	if rest, ok := bytes.CutPrefix(value, []byte(partsPrefix)); ok {
		count, _, _ := bytes.Cut(rest, []byte(":"))
		if n, err := strconv.Atoi(string(count)); err == nil && n <= maxParts {
			return n
		}
	}
	return 1
}

func (k *Keychain) putItem(ctx context.Context, service, value string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := fmt.Sprintf("add-generic-password -U -a %s -s %s -l %s -w %s\n", k.Account, service, service, value)
	cmd := exec.CommandContext(ctx, k.binary, "-i")
	cmd.Stdin = strings.NewReader(command)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil || stderr.Len() > 0 {
		return fmt.Errorf("写入钥匙串失败（%s）", service)
	}
	// Interactive mode does not report failures through the exit code; confirm by reading back.
	stored, err := k.readItem(ctx, service)
	if err != nil || string(stored) != value {
		return fmt.Errorf("写入钥匙串后校验失败（%s）", service)
	}
	return nil
}

func (k *Keychain) Delete(ctx context.Context, service string) error {
	if !servicePattern.MatchString(service) {
		return errors.New("钥匙串条目名称格式无效")
	}
	for i := k.partCount(ctx, service); i >= 2; i-- {
		if err := k.deleteItem(ctx, partName(service, i)); err != nil {
			return err
		}
	}
	return k.deleteItem(ctx, service)
}

func (k *Keychain) deleteItem(ctx context.Context, service string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, k.binary, "delete-generic-password", "-a", k.Account, "-s", service).Run()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 44) {
		return fmt.Errorf("删除钥匙串条目失败（%s）", service)
	}
	return nil
}
