package sftp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/encoding"
)

// FileResult holds the result of a file_read operation.
type FileResult struct {
	Data      string `json:"data,omitempty"`
	Mode      string `json:"mode"` // "text" | "hex" | "file"
	Encoding  string `json:"encoding,omitempty"`
	TotalSize int64  `json:"total_size"`
	HasMore   bool   `json:"has_more,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	Length    int64  `json:"length,omitempty"`
	BytesRead int64  `json:"bytes_read,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
}

// FileStatResult holds the result of a file_stat operation.
type FileStatResult struct {
	Name     string              `json:"name"`
	Size     int64               `json:"size"`
	IsDir    bool                `json:"is_dir"`
	ModTime  string              `json:"mod_time,omitempty"`
	Children []FileStatResult    `json:"children,omitempty"`
}

// sftpClient abstracts SFTP operations.
type Client struct {
	client *sftp.Client
}

// Close closes the SFTP client.
func (s *Client) Close() error {
	return s.client.Close()
}

// RemoveFile deletes a remote file.
func (s *Client) RemoveFile(remotePath string) error {
	return s.client.Remove(remotePath)
}

// RenameFile moves or renames a remote file/directory (same filesystem).
func (s *Client) RenameFile(oldPath, newPath string) error {
	return s.client.Rename(oldPath, newPath)
}

// MakeDir creates a directory (and parents) on the remote.
func (s *Client) MakeDir(remotePath string) error {
	return s.client.MkdirAll(remotePath)
}


// StreamReadTo reads from remotePath (optionally at offset for length bytes) and streams
// raw bytes into w. Uses io.CopyN for zero-buffer streaming — never loads the whole file
// into memory. For large files use offset=0, length<=0 to stream the entire file.
func (s *Client) StreamReadTo(w io.Writer, remotePath string, offset, length int64) (int64, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat remote file: %w", err)
	}

	if offset < 0 {
		offset = 0
	}
	if length <= 0 || offset+length > fi.Size() {
		length = fi.Size() - offset
	}
	if length < 0 {
		length = 0
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}

	return io.CopyN(w, f, length)
}

// StreamWriteFrom reads raw bytes from r and writes them to remotePath at the given
// offset. offset=0 truncates the file; offset>0 writes at that position without
// truncation. Uses io.Copy to stream without buffering in memory.
func (s *Client) StreamWriteFrom(r io.Reader, remotePath string, offset int64) (int64, error) {
	flag := os.O_RDWR | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	f, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}

	return io.Copy(f, r)
}

// ReadFile reads a file (or segment) from the remote.
func (s *Client) ReadFile(remotePath string, offset, length int64, mode string, localPath string) (*FileResult, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat remote file: %w", err)
	}
	totalSize := fi.Size()

	if offset < 0 {
		offset = 0
	}
	if length <= 0 || offset+length > totalSize {
		length = totalSize - offset
	}

	result := &FileResult{
		Mode:      mode,
		TotalSize: totalSize,
		Offset:    offset,
		Length:    length,
		HasMore:   offset+length < totalSize,
	}

	// File mode: write to local file.
	if mode == "file" {
		if localPath == "" {
			return nil, fmt.Errorf("local_path required for file mode")
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek: %w", err)
		}
		localFile, err := os.Create(localPath)
		if err != nil {
			return nil, fmt.Errorf("create local file: %w", err)
		}
		n, err := io.CopyN(localFile, f, length)
		localFile.Close()
		if err != nil {
			return nil, fmt.Errorf("copy to local: %w", err)
		}
		result.BytesRead = n
		result.LocalPath = localPath
		return result, nil
	}

	// Read the segment.
	buf := make([]byte, length)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, fmt.Errorf("read: %w", err)
	}
	segment := buf[:n]
	result.BytesRead = int64(n)
	result.Length = int64(n)

	// Encode based on mode.
	switch mode {
	case "hex":
		result.Data = fmt.Sprintf("%x", segment)
	case "text", "":
		result.Data = encoding.EncodeText(segment)
	default:
		return nil, fmt.Errorf("unknown mode %q (use text, hex, or file)", mode)
	}

	return result, nil
}

// WriteFile writes data to a remote file, either inline or from a local file.
func (s *Client) WriteFile(remotePath string, offset int64, data string, mode string, localPath string, localOffset, length int64) (int64, error) {
	if localPath != "" {
		return s.writeFromLocal(remotePath, offset, localPath, localOffset, length)
	}
	return s.writeInline(remotePath, offset, data, mode)
}

func (s *Client) writeInline(remotePath string, offset int64, data string, mode string) (int64, error) {
	var raw []byte
	switch mode {
	case "hex":
		decoded, err := encoding.HexDecode(data)
		if err != nil {
			return 0, fmt.Errorf("hex decode: %w", err)
		}
		raw = decoded
	case "text", "":
		raw = encoding.DecodeText(data)
	default:
		return 0, fmt.Errorf("unknown mode %q", mode)
	}

	flag := os.O_RDWR | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	f, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}
	n, err := f.Write(raw)
	if err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}
	return int64(n), nil
}

func (s *Client) writeFromLocal(remotePath string, offset int64, localPath string, localOffset, length int64) (int64, error) {
	localFile, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("open local file: %w", err)
	}
	defer localFile.Close()

	fi, err := localFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat local file: %w", err)
	}
	if length <= 0 || localOffset+length > fi.Size() {
		length = fi.Size() - localOffset
	}
	if _, err := localFile.Seek(localOffset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek local: %w", err)
	}

	flag := os.O_RDWR | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	remoteFile, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer remoteFile.Close()

	if offset > 0 {
		if _, err := remoteFile.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek remote: %w", err)
		}
	}

	n, err := io.CopyN(remoteFile, localFile, length)
	if err != nil {
		return 0, fmt.Errorf("copy: %w", err)
	}
	return n, nil
}

// StatFile returns file/directory info.
func (s *Client) StatFile(remotePath string) (*FileStatResult, error) {
	fi, err := s.client.Stat(remotePath)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	result := &FileStatResult{
		Name:    filepath.Base(remotePath),
		Size:    fi.Size(),
		IsDir:   fi.IsDir(),
		ModTime: fi.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
	}

	if fi.IsDir() {
		entries, err := s.client.ReadDir(remotePath)
		if err != nil {
			return result, nil // partial result
		}
		for _, e := range entries {
			result.Children = append(result.Children, FileStatResult{
				Name:    e.Name(),
				Size:    e.Size(),
				IsDir:   e.IsDir(),
				ModTime: e.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
	}

	return result, nil
}

// ChmodFile changes the permissions of a remote file.
func (s *Client) ChmodFile(remotePath string, mode os.FileMode) error {
	return s.client.Chmod(remotePath, mode)
}

// ChownFile changes the owner and group of a remote file.
func (s *Client) ChownFile(remotePath string, uid, gid int) error {
	return s.client.Chown(remotePath, uid, gid)
}

// ChtimesFile changes the access and modification times of a remote file.
func (s *Client) ChtimesFile(remotePath string, atime, mtime time.Time) error {
	return s.client.Chtimes(remotePath, atime, mtime)
}

// ReadLink returns the target of a symbolic link.
func (s *Client) ReadLink(path string) (string, error) {
	return s.client.ReadLink(path)
}

// SymlinkFile creates a symbolic link on the remote.
// target is the existing path, linkPath is the new symlink to create.
func (s *Client) SymlinkFile(target, linkPath string) error {
	return s.client.Symlink(target, linkPath)
}

// LinkFile creates a hard link on the remote.
func (s *Client) LinkFile(existing, newPath string) error {
	return s.client.Link(existing, newPath)
}

// TruncateFile truncates a remote file to the given size.
func (s *Client) TruncateFile(path string, size int64) error {
	return s.client.Truncate(path, size)
}

// RealPath returns the canonical absolute path of a remote file or directory.
func (s *Client) RealPath(path string) (string, error) {
	return s.client.RealPath(path)
}

// FsStatResult holds the result of a file_statvfs operation.
type FsStatResult struct {
	TotalSpace  uint64 `json:"total_space"`
	FreeSpace   uint64 `json:"free_space"`
	AvailSpace  uint64 `json:"avail_space"`
	TotalINodes uint64 `json:"total_inodes"`
	FreeINodes  uint64 `json:"free_inodes"`
	AvailINodes uint64 `json:"avail_inodes"`
	BlockSize   uint64 `json:"block_size"`
}

// fsStatFromVFS converts a pkg/sftp StatVFS to FsStatResult.
func fsStatFromVFS(v *sftp.StatVFS) *FsStatResult {
	return &FsStatResult{
		TotalSpace:  v.Frsize * v.Blocks,
		FreeSpace:   v.Frsize * v.Bfree,
		AvailSpace:  v.Frsize * v.Bavail,
		TotalINodes: v.Files,
		FreeINodes:  v.Ffree,
		AvailINodes: v.Favail,
		BlockSize:   v.Frsize,
	}
}

// StatVFS returns filesystem statistics for the given remote path.
func (s *Client) StatVFS(path string) (*FsStatResult, error) {
	v, err := s.client.StatVFS(path)
	if err != nil {
		return nil, fmt.Errorf("statvfs: %w", err)
	}
	return fsStatFromVFS(v), nil
}

// Getwd returns the remote working directory.
func (s *Client) Getwd() (string, error) {
	return s.client.Getwd()
}

// OpenSFTPOverSSH opens an SFTP connection over an existing SSH client (for regular SSH).
func NewClient(sshClient *ssh.Client) (*Client, error) {
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return &Client{client: sftpCli}, nil
}
