package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gitleaks/go-gitdiff/gitdiff"
	"github.com/go-errors/errors"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog"
	log "github.com/sirupsen/logrus"
	glgo "github.com/zricethezav/gitleaks/v8/detect/git"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/source_metadatapb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/sourcespb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sanitizer"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sources"
)

type Source struct {
	name     string
	sourceId int64
	jobId    int64
	verify   bool
	git      *Git
	aCtx     context.Context
	sources.Progress
	conn *sourcespb.Git
}

type Git struct {
	sourceType         sourcespb.SourceType
	sourceName         string
	sourceID           int64
	jobID              int64
	sourceMetadataFunc func(file, email, commit, timestamp, repository string, line int64) *source_metadatapb.MetaData
	verify             bool
	// sem is used to limit concurrency
	sem *semaphore.Weighted
}

func NewGit(sourceType sourcespb.SourceType, jobID, sourceID int64, sourceName string, verify bool, concurrency int,
	sourceMetadataFunc func(file, email, commit, timestamp, repository string, line int64) *source_metadatapb.MetaData,
) *Git {
	return &Git{
		sourceType:         sourceType,
		sourceName:         sourceName,
		sourceID:           sourceID,
		jobID:              jobID,
		sourceMetadataFunc: sourceMetadataFunc,
		verify:             verify,
		sem:                semaphore.NewWeighted(int64(concurrency)),
	}
}

// Ensure the Source satisfies the interface at compile time
var _ sources.Source = (*Source)(nil)

// Type returns the type of source.
// It is used for matching source types in configuration and job input.
func (s *Source) Type() sourcespb.SourceType {
	return sourcespb.SourceType_SOURCE_TYPE_GIT
}

func (s *Source) SourceID() int64 {
	return s.sourceId
}

func (s *Source) JobID() int64 {
	return s.jobId
}

// Init returns an initialized GitHub source.
func (s *Source) Init(aCtx context.Context, name string, jobId, sourceId int64, verify bool, connection *anypb.Any, concurrency int) error {

	s.aCtx = aCtx
	s.name = name
	s.sourceId = sourceId
	s.jobId = jobId
	s.verify = verify

	var conn sourcespb.Git
	err := anypb.UnmarshalTo(connection, &conn, proto.UnmarshalOptions{})
	if err != nil {
		errors.WrapPrefix(err, "error unmarshalling connection", 0)
	}

	s.conn = &conn

	s.git = NewGit(s.Type(), s.jobId, s.sourceId, s.name, s.verify, runtime.NumCPU(),
		func(file, email, commit, repository, timestamp string, line int64) *source_metadatapb.MetaData {
			return &source_metadatapb.MetaData{
				Data: &source_metadatapb.MetaData_Git{
					Git: &source_metadatapb.Git{
						Commit:     sanitizer.UTF8(commit),
						File:       sanitizer.UTF8(file),
						Email:      sanitizer.UTF8(email),
						Repository: sanitizer.UTF8(repository),
						Timestamp:  sanitizer.UTF8(timestamp),
						Line:       line,
					},
				},
			}
		})
	return nil
}

// Chunks emits chunks of bytes over a channel.
func (s *Source) Chunks(ctx context.Context, chunksChan chan *sources.Chunk) error {
	switch cred := s.conn.GetCredential().(type) {
	case *sourcespb.Git_BasicAuth:
		user := cred.BasicAuth.Username
		token := cred.BasicAuth.Password

		for i, repoURI := range s.conn.Repositories {
			s.SetProgressComplete(i, len(s.conn.Repositories), fmt.Sprintf("Repo: %s", repoURI), "")
			if len(repoURI) == 0 {
				continue
			}
			path, repo, err := CloneRepoUsingToken(token, repoURI, user)
			defer os.RemoveAll(path)
			if err != nil {
				return err
			}
			err = s.git.ScanRepo(ctx, repo, path, NewScanOptions(), chunksChan)
			if err != nil {
				return err
			}
		}
	case *sourcespb.Git_Unauthenticated:
		for i, repoURI := range s.conn.Repositories {
			s.SetProgressComplete(i, len(s.conn.Repositories), fmt.Sprintf("Repo: %s", repoURI), "")
			if len(repoURI) == 0 {
				continue
			}
			path, repo, err := CloneRepoUsingUnauthenticated(repoURI)
			defer os.RemoveAll(path)
			if err != nil {
				return err
			}
			err = s.git.ScanRepo(ctx, repo, path, NewScanOptions(), chunksChan)
			if err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid connection type for git source")
	}

	for i, u := range s.conn.Directories {
		s.SetProgressComplete(i, len(s.conn.Repositories), fmt.Sprintf("Repo: %s", u), "")

		if len(u) == 0 {
			continue
		}
		if !strings.HasSuffix(u, "git") {
			//try paths instead of url
			repo, err := RepoFromPath(u)
			if err != nil {
				return err
			}
			if strings.HasPrefix(u, filepath.Join(os.TempDir(), "trufflehog")) {
				defer os.RemoveAll(u)
			}

			err = s.git.ScanRepo(ctx, repo, u, NewScanOptions(), chunksChan)
			if err != nil {
				return err

			}
		}

	}
	return nil
}

func RepoFromPath(path string) (*git.Repository, error) {
	return git.PlainOpen(path)
}

func CleanOnError(err *error, path string) {
	if *err != nil {
		os.RemoveAll(path)
	}
}

func CloneRepo(userInfo *url.Userinfo, gitUrl string) (clonePath string, repo *git.Repository, err error) {
	if err = GitCmdCheck(); err != nil {
		return
	}
	clonePath, err = ioutil.TempDir(os.TempDir(), "trufflehog")
	if err != nil {
		err = errors.New(err)
		return
	}
	defer CleanOnError(&err, clonePath)
	cloneURL, err := url.Parse(gitUrl)
	if err != nil {
		err = errors.WrapPrefix(err, "could not parse url", 0)
		return
	}

	cloneURL.User = userInfo

	cloneCmd := exec.Command("git", "clone", cloneURL.String(), clonePath)
	if err := cloneCmd.Run(); err != nil {
		err = errors.WrapPrefix(err, "error running 'git clone'", 0)
	}

	repo, err = git.PlainOpen(clonePath)
	if err != nil {
		err = errors.WrapPrefix(err, "could not open repo", 0)
		return
	}
	return
}

func CloneRepoUsingToken(token, gitUrl, user string) (string, *git.Repository, error) {
	userInfo := url.UserPassword(user, token)
	return CloneRepo(userInfo, gitUrl)
}

func CloneRepoUsingUnauthenticated(url string) (string, *git.Repository, error) {
	return CloneRepo(nil, url)
}

func GitCmdCheck() error {
	if errors.Is(exec.Command("git").Run(), exec.ErrNotFound) {
		return fmt.Errorf("'git' command not found in $PATH. Make sure git is installed and included in $PATH")
	}
	return nil
}

func (s *Git) ScanCommits(repo *git.Repository, path string, scanOptions *ScanOptions, chunksChan chan *sources.Chunk) error {
	if err := GitCmdCheck(); err != nil {
		return err
	}
	zerolog.SetGlobalLevel(zerolog.Disabled)
	fileChan, err := glgo.GitLog(path, scanOptions.HeadHash)
	if err != nil {
		return errors.WrapPrefix(err, "could not open repo path", 0)
	}
	var depth int64
	for file := range fileChan {
		if scanOptions.MaxDepth > 0 && depth >= scanOptions.MaxDepth {
			log.Debugf("reached max depth")
			break
		}
		depth++
		if len(scanOptions.BaseHash) > 0 {
			if file.PatchHeader.SHA == scanOptions.BaseHash {
				log.Debugf("reached base commit")
				break
			}
		}
		if !scanOptions.Filter.Pass(file.NewName) {
			continue
		}

		fileName := file.NewName
		if fileName == "" {
			continue
		}
		var email, hash, when string
		if file.PatchHeader != nil {
			if file.PatchHeader.Author != nil {
				email = file.PatchHeader.Author.Email
			}
			hash = file.PatchHeader.SHA
			when = file.PatchHeader.AuthorDate.String()
		}

		remote, err := repo.Remote("origin")
		if err != nil {
			return errors.WrapPrefix(err, "error getting repo remote origin", 0)
		}
		safeRepo, err := stripPassword(remote.Config().URLs[0])
		if err != nil {
			return errors.WrapPrefix(err, "couldn't get repo name", 0)
		}

		for _, frag := range file.TextFragments {
			newLines := bytes.Buffer{}
			newLineNumber := frag.NewPosition
			for _, line := range frag.Lines {
				if line.Op == gitdiff.OpAdd {
					newLines.WriteString(strings.ReplaceAll(line.String(), "\n", " ") + "\n")
				}
			}
			metadata := s.sourceMetadataFunc(fileName, email, hash, when, safeRepo, newLineNumber)
			chunksChan <- &sources.Chunk{
				SourceName:     s.sourceName,
				SourceID:       s.sourceID,
				SourceType:     s.sourceType,
				SourceMetadata: metadata,
				Data:           newLines.Bytes(),
				Verify:         s.verify,
			}
		}
	}
	return nil
}

func (s *Git) ScanUnstaged(repo *git.Repository, scanOptions *ScanOptions, chunksChan chan *sources.Chunk) error {
	remote, err := repo.Remote("origin")
	if err != nil {
		return errors.New(err)
	}
	safeRepo, err := stripPassword(remote.Config().URLs[0])
	if err != nil {
		return errors.New(err)
	}

	// Also scan any unstaged changes in the working tree of the repo
	_, err = repo.Head()
	if err == nil || err == plumbing.ErrReferenceNotFound {
		wt, err := repo.Worktree()
		if err != nil {
			log.WithError(err).Error("error obtaining repo worktree")
			return err
		}

		status, err := wt.Status()
		if err != nil {
			log.WithError(err).Error("error obtaining worktree status")
			return err
		}
		for fh := range status {
			if !scanOptions.Filter.Pass(fh) {
				continue
			}
			metadata := s.sourceMetadataFunc(
				fh, "unstaged", "unstaged", time.Now().String(), safeRepo, 0,
			)

			fileBuf := bytes.NewBuffer(nil)
			fileHandle, err := wt.Filesystem.Open(fh)
			if err != nil {
				continue
			}
			defer fileHandle.Close()
			_, err = io.Copy(fileBuf, fileHandle)
			if err != nil {
				continue
			}
			chunksChan <- &sources.Chunk{
				SourceType:     s.sourceType,
				SourceName:     s.sourceName,
				SourceID:       s.sourceID,
				Data:           fileBuf.Bytes(),
				SourceMetadata: metadata,
				Verify:         s.verify,
			}
		}
	}
	return nil
}

func (s *Git) ScanRepo(ctx context.Context, repo *git.Repository, repoPath string, scanOptions *ScanOptions, chunksChan chan *sources.Chunk) error {
	start := time.Now().UnixNano()
	if err := s.ScanCommits(repo, repoPath, scanOptions, chunksChan); err != nil {
		return err
	}
	if err := s.ScanUnstaged(repo, scanOptions, chunksChan); err != nil {
		// https://github.com/src-d/go-git/issues/879
		if strings.Contains(err.Error(), "object not found") {
			log.WithError(err).Error("known issue: probably caused by a dangling reference in the repo")
		} else {
			return errors.New(err)
		}
		return err
	}
	scanTime := time.Now().UnixNano() - start
	log.Debugf("Scanning complete. Scan time: %f", time.Duration(scanTime).Seconds())
	return nil
}

//GenerateLink crafts a link to the specific file from a commit. This works in most major git providers (Github/Gitlab)
func GenerateLink(repo, commit, file string) string {
	//bitbucket links are commits not commit...
	if strings.Contains(repo, "bitbucket.org/") {
		return repo[:len(repo)-4] + "/commits/" + commit
	}
	link := repo[:len(repo)-4] + "/blob/" + commit + "/" + file

	if file == "" {
		link = repo[:len(repo)-4] + "/commit/" + commit
	}
	return link
}

func stripPassword(u string) (string, error) {
	if strings.HasPrefix(u, "git@") {
		return u, nil
	}

	repoURL, err := url.Parse(u)
	if err != nil {
		return "", errors.WrapPrefix(err, "repo remote cannot be sanitized as URI", 0)
	}

	repoURL.User = nil

	return repoURL.String(), nil
}

func TryAdditionalBaseRefs(repo *git.Repository, base string) (*plumbing.Hash, error) {
	revisionPrefixes := []string{
		"",
		"refs/heads/",
		"refs/remotes/origin/",
	}
	for _, prefix := range revisionPrefixes {
		outHash, err := repo.ResolveRevision(plumbing.Revision(prefix + base))
		if err == plumbing.ErrReferenceNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		return outHash, nil
	}

	return nil, fmt.Errorf("no base refs succeeded for base: %q", base)
}
func PrepareRepo(uriString string) (string, bool, error) {
	var path string
	uri, err := url.Parse(uriString)
	if err != nil {
		return "", false, fmt.Errorf("unable to parse Git URI: %s", err)
	}

	remote := false
	switch uri.Scheme {
	case "file":
		path = fmt.Sprintf("%s%s", uri.Host, uri.Path)
	case "https":
		remotePath := fmt.Sprintf("%s://%s%s", uri.Scheme, uri.Host, uri.Path)
		remote = true
		switch {
		case uri.User != nil:
			log.Debugf("Cloning remote Git repo with authentication")
			password, ok := uri.User.Password()
			if !ok {
				return "", remote, fmt.Errorf("password must be included in Git repo URL when username is provided")
			}
			path, _, err = CloneRepoUsingToken(password, remotePath, uri.User.Username())
			if err != nil {
				return path, remote, fmt.Errorf("failed to clone authenticated Git repo (%s): %s", remotePath, err)
			}
		default:
			log.Debugf("Cloning remote Git repo without authentication")
			path, _, err = CloneRepoUsingUnauthenticated(remotePath)
			if err != nil {
				return path, remote, fmt.Errorf("failed to clone unauthenticated Git repo (%s): %s", remotePath, err)
			}
		}
	default:
		return "", remote, fmt.Errorf("unsupported Git URI: %s", uriString)
	}
	log.Debugf("Git repo local path: %s", path)
	return path, remote, nil
}
