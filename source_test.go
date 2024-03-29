package dalec

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/image"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestSourceGitSSH(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// buildkit's llb.Git currently directly runs an ssh keyscan (connects to the specified host and port to get the host key)
	// This is not ideal for our test setup here because we need to use a real server.
	// Thankfully when there is an error it is ignored so we don't actually need to setup a full SSH server here.
	addr := stubListener(t)

	src := Source{
		Git: &SourceGit{
			URL:    fmt.Sprintf("user@%s:test.git", addr),
			Commit: t.Name(),
		},
	}

	ops := getSourceOp(ctx, t, src)
	checkGitOp(t, ops, &src)

	t.Run("with subdir", func(t *testing.T) {
		src := src
		src.Path = "subdir"
		ops2 := getSourceOp(ctx, t, src)
		checkGitOp(t, ops2, &src)

		// git ops require extra filtering to get the correct subdir, so we should have an extra op
		if len(ops2) != len(ops)+1 {
			t.Fatalf("expected %d ops, got %d", len(ops)+1, len(ops2))
		}

		checkFilter(t, ops2[1].GetFile(), &src)
	})

	t.Run("with include-exclude", func(t *testing.T) {
		src := src
		src.Includes = []string{"foo", "bar"}
		src.Excludes = []string{"baz"}
		ops2 := getSourceOp(ctx, t, src)
		checkGitOp(t, ops2, &src)

		// git ops require extra filtering to get the correct subdir, so we should have an extra op
		if len(ops2) != len(ops)+1 {
			t.Fatalf("expected %d ops, got %d", len(ops)+1, len(ops2))
		}

		checkFilter(t, ops2[1].GetFile(), &src)
	})

	t.Run("with include-exclude and subpath", func(t *testing.T) {
		src := src
		src.Includes = []string{"foo", "bar"}
		src.Excludes = []string{"baz"}
		src.Path = "subdir"

		ops2 := getSourceOp(ctx, t, src)
		checkGitOp(t, ops2, &src)

		// git ops require extra filtering to get the correct subdir, so we should have an extra op
		if len(ops2) != len(ops)+1 {
			t.Fatalf("expected %d ops, got %d", len(ops)+1, len(ops2))
		}

		checkFilter(t, ops2[1].GetFile(), &src)
	})
}

func TestSourceGitHTTP(t *testing.T) {
	t.Parallel()

	src := Source{
		Git: &SourceGit{
			URL:    "https://localhost/test.git",
			Commit: t.Name(),
		},
	}

	ctx := context.Background()
	ops := getSourceOp(ctx, t, src)
	checkGitOp(t, ops, &src)

	t.Run("with subdir", func(t *testing.T) {
		src := src
		src.Path = "subdir"
		ops2 := getSourceOp(ctx, t, src)
		checkGitOp(t, ops2, &src)

		// git ops require extra filtering to get the correct subdir, so we should have an extra op
		if len(ops2) != len(ops)+1 {
			t.Fatalf("expected %d ops, got %d", len(ops)+1, len(ops2))
		}

		checkFilter(t, ops2[1].GetFile(), &src)
	})

	t.Run("with include-exclude", func(t *testing.T) {
		src := src
		src.Includes = []string{"foo", "bar"}
		src.Excludes = []string{"baz"}
		ops2 := getSourceOp(ctx, t, src)
		checkGitOp(t, ops2, &src)

		// git ops require extra filtering to get the correct subdir, so we should have an extra op
		if len(ops2) != len(ops)+1 {
			t.Fatalf("expected %d ops, got %d", len(ops)+1, len(ops2))
		}

		checkFilter(t, ops2[1].GetFile(), &src)
	})

	t.Run("with include-exclude and subpath", func(t *testing.T) {
		src := src
		src.Includes = []string{"foo", "bar"}
		src.Excludes = []string{"baz"}
		src.Path = "subdir"

		ops2 := getSourceOp(ctx, t, src)
		checkGitOp(t, ops2, &src)

		// git ops require extra filtering to get the correct subdir, so we should have an extra op
		if len(ops2) != len(ops)+1 {
			t.Fatalf("expected %d ops, got %d", len(ops)+1, len(ops2))
		}

		checkFilter(t, ops2[1].GetFile(), &src)
	})
}

func TestSourceHTTP(t *testing.T) {
	src := Source{
		HTTP: &SourceHTTP{
			URL: "https://localhost/test.tar.gz",
		},
	}

	ctx := context.Background()
	ops := getSourceOp(ctx, t, src)

	op := ops[0].GetSource()

	xID := "https://localhost/test.tar.gz"
	if op.Identifier != xID {
		t.Errorf("expected identifier %q, got %q", xID, op.Identifier)
	}

	if len(op.Attrs) != 1 {
		t.Errorf("expected 1 attribute, got %d", len(op.Attrs))
	}

	// For http sources we expect the filename to be the name of the source not the filename in the URL.
	xFilename := "test"
	const httpFilename = "http.filename"
	if op.Attrs[httpFilename] != "test" {
		t.Errorf("expected http.filename %q, got %q", xFilename, op.Attrs[httpFilename])
	}
}

func TestSourceDockerImage(t *testing.T) {
	imgRef := "localhost:0/does/not/exist:latest"
	src := Source{
		DockerImage: &SourceDockerImage{
			Ref: imgRef,
		},
	}
	ctx := context.Background()
	ops := getSourceOp(ctx, t, src)

	op := ops[0].GetSource()

	xID := "docker-image://" + imgRef
	if op.Identifier != xID {
		t.Errorf("expected identifier %q, got %q", xID, op.Identifier)
	}

	t.Run("with cmd", func(t *testing.T) {
		src := Source{
			DockerImage: &SourceDockerImage{
				Ref: imgRef,
				Cmd: &Command{
					Dir: "/tmp",
					Steps: []*BuildStep{
						{Command: "echo hello 1", Env: map[string]string{"FOO": "bar1"}},
						{Command: "echo hello 2", Env: map[string]string{"FOO": "bar2"}},
					},
				},
			},
		}

		ctx := context.Background()
		ops := getSourceOp(ctx, t, src)

		img := ops[0].GetSource()
		if img.Identifier != xID {
			t.Errorf("expected identifier %q, got %q", xID, img.Identifier)
		}
		checkCmd(t, ops[1:], &src)

		t.Run("with filters", func(t *testing.T) {
			t.Run("include and exclude", func(t *testing.T) {
				src := src
				src.Includes = []string{"foo", "bar"}
				src.Excludes = []string{"baz"}

				ops := getSourceOp(ctx, t, src)
				checkCmd(t, ops[1:len(ops)-1], &src)
				// When include/exclude are used, we are expecting a copy operation to be last.
				checkFilter(t, ops[len(ops)-1].GetFile(), &src)
			})
			t.Run("subpath", func(t *testing.T) {
				src := src
				src.Path = "subdir"

				ops := getSourceOp(ctx, t, src)

				img := ops[0].GetSource()
				if img.Identifier != xID {
					t.Errorf("expected identifier %q, got %q", xID, img.Identifier)
				}

				checkCmd(t, ops[1:], &src)
			})

			t.Run("subpath with include-exclude", func(t *testing.T) {
				src := src
				src.Path = "subdir"
				src.Includes = []string{"foo", "bar"}
				src.Excludes = []string{"baz"}

				ops := getSourceOp(ctx, t, src)

				img := ops[0].GetSource()
				if img.Identifier != xID {
					t.Errorf("expected identifier %q, got %q", xID, img.Identifier)
				}
				ops = ops[1:]

				// last op is (should be) the include/exclude filter and not a cmd
				checkCmd(t, ops[:len(ops)-1], &src)
				// When include/exclude are used, we are expecting a copy operation to be last.
				checkFilter(t, ops[len(ops)-1].GetFile(), &src)
			})
		})
	})
}

func TestSourceBuild(t *testing.T) {
	src := Source{
		Build: &SourceBuild{
			Inline: `
FROM busybox:latest
RUN echo hello
`,
		},
	}

	ctx := context.Background()
	ops := getSourceOp(ctx, t, src)

	xID := "docker-image://docker.io/library/busybox:latest"
	id := ops[0].GetSource().Identifier
	if id != xID {
		t.Errorf("expected identifier %q, got %q", xID, id)
	}

	// To reuse code, let's craft an equivelant SourceDockerImage with cmd's
	// We'll use that to validate the ops we got from the build source with [checkCmd]
	srcDI := SourceDockerImage{
		Ref: xID,
		Cmd: &Command{
			Dir: "/", // Dockerfile defaults to /
			Env: map[string]string{
				// The dockerfile frontend auto-injects these env vars
				"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			},
			Steps: []*BuildStep{
				{Command: "echo hello"},
			},
		},
	}

	checkCmd(t, ops[1:], &Source{DockerImage: &srcDI})

	t.Run("with filters", func(t *testing.T) {
		t.Run("subdir", func(t *testing.T) {
			src := src
			src.Path = "subdir"

			// for build soruce, we expect to have a copy operation as the last op
			ops := getSourceOp(ctx, t, src)
			checkCmd(t, ops[1:len(ops)-1], &Source{DockerImage: &srcDI})
			checkFilter(t, ops[len(ops)-1].GetFile(), &src)
		})

		t.Run("include and exclude", func(t *testing.T) {
			src := src
			src.Includes = []string{"foo", "bar"}
			src.Excludes = []string{"baz"}

			// for build soruce, we expect to have a copy operation as the last op
			ops := getSourceOp(ctx, t, src)
			checkCmd(t, ops[1:len(ops)-1], &Source{DockerImage: &srcDI})
			checkFilter(t, ops[len(ops)-1].GetFile(), &src)
		})

		t.Run("subpath with include-exclude", func(t *testing.T) {
			src := src
			src.Path = "subdir"
			src.Includes = []string{"foo", "bar"}
			src.Excludes = []string{"baz"}

			// for build soruce, we expect to have a copy operation as the last op
			ops := getSourceOp(ctx, t, src)
			checkCmd(t, ops[1:len(ops)-1], &Source{DockerImage: &srcDI})
			checkFilter(t, ops[len(ops)-1].GetFile(), &src)
		})
	})
}

func TestSourceContext(t *testing.T) {
	ctx := context.Background()

	testWithFilters := func(t *testing.T, src Source) {
		t.Run("with filters", func(t *testing.T) {
			t.Run("subdir", func(t *testing.T) {
				src := src
				src.Path = "subdir"
				ops := getSourceOp(ctx, t, src)
				checkContext(t, ops[0].GetSource(), &src)
				// for context soruce, we expect to have a copy operation as the last op when subdir is used
				checkFilter(t, ops[1].GetFile(), &src)
			})

			t.Run("include and exclude", func(t *testing.T) {
				src := src
				src.Includes = []string{"foo", "bar"}
				src.Excludes = []string{"baz"}
				ops := getSourceOp(ctx, t, src)
				checkContext(t, ops[0].GetSource(), &src)
				// With include/exclude only, this should be handled with just one op.
				if len(ops) != 1 {
					t.Fatalf("expected 1 op, got %d\n%s", len(ops), ops)
				}
			})

			t.Run("subpath with include-exclude", func(t *testing.T) {
				src := src
				src.Path = "subdir"
				src.Includes = []string{"foo", "bar"}
				src.Excludes = []string{"baz"}
				ops := getSourceOp(ctx, t, src)
				checkContext(t, ops[0].GetSource(), &src)
				// for context soruce, we expect to have a copy operation as the last op when subdir is used
				checkFilter(t, ops[1].GetFile(), &src)
			})
		})
	}

	t.Run("default", func(t *testing.T) {
		src := Source{
			Context: &SourceContext{},
		}
		ops := getSourceOp(ctx, t, src)
		checkContext(t, ops[0].GetSource(), &src)

		testWithFilters(t, src)
	})

	t.Run("with customn name", func(t *testing.T) {
		src := Source{
			Context: &SourceContext{Name: "some-name"},
		}
		ops := getSourceOp(ctx, t, src)
		checkContext(t, ops[0].GetSource(), &src)
		testWithFilters(t, src)
	})
}

func TestSourceInlineFile(t *testing.T) {
	ctx := context.Background()

	for name, f := range testFiles() {
		f := f
		t.Run(name, func(t *testing.T) {
			src := Source{Inline: &SourceInline{File: f}}
			ops := getSourceOp(ctx, t, src)
			if len(ops) != 1 {
				t.Fatalf("expected 1 op, got %d:\n%s", len(ops), ops)
			}
			checkMkfile(t, ops[0].GetFile(), src.Inline.File, "/test")
		})
	}
}

func testFiles() map[string]*SourceInlineFile {
	empty := func() *SourceInlineFile {
		return &SourceInlineFile{}
	}

	modify := func(mods ...func(*SourceInlineFile)) *SourceInlineFile {
		src := empty()
		for _, mod := range mods {
			mod(src)
		}
		return src
	}

	withUID := func(uid int) func(*SourceInlineFile) {
		return func(s *SourceInlineFile) {
			s.UID = uid
		}
	}

	withGID := func(gid int) func(*SourceInlineFile) {
		return func(s *SourceInlineFile) {
			s.GID = gid
		}
	}

	withContents := func(contents string) func(*SourceInlineFile) {
		return func(s *SourceInlineFile) {
			s.Contents = contents
		}
	}

	return map[string]*SourceInlineFile{
		"empty file":                  modify(),
		"empty file with uid":         modify(withUID(1000)),
		"empty file with gid":         modify(withGID(1000)),
		"empty file with uid and gid": modify(withUID(1000), withGID(1000)),
		"with contents":               modify(withContents("hello world 1")),
		"with uid and contents":       modify(withUID(1000), withContents("hello world 2")),
		"with gid and contents":       modify(withGID(1000), withContents("hello world 3")),
		"with uid, gid, and contents": modify(withUID(1000), withGID(1000), withContents("hello world 4")),
	}
}

func TestSourceInlineDir(t *testing.T) {
	ctx := context.Background()

	empty := func() *SourceInlineDir {
		return &SourceInlineDir{}
	}

	modify := func(mods ...func(*SourceInlineDir)) *SourceInlineDir {
		src := empty()
		for _, mod := range mods {
			mod(src)
		}
		return src
	}

	withDirUID := func(uid int) func(*SourceInlineDir) {
		return func(s *SourceInlineDir) {
			s.UID = uid
		}
	}

	withDirGID := func(gid int) func(*SourceInlineDir) {
		return func(s *SourceInlineDir) {
			s.GID = gid
		}
	}

	testDirs := map[string]*SourceInlineDir{
		"default":          modify(),
		"with uid":         modify(withDirUID(1000)),
		"with gid":         modify(withDirGID(1000)),
		"with uid and gid": modify(withDirUID(1000), withDirGID(1001)),
	}

	for name, dir := range testDirs {
		dir := dir
		t.Run(name, func(t *testing.T) {
			src := Source{Inline: &SourceInline{Dir: dir}}
			ops := getSourceOp(ctx, t, src)
			checkMkdir(t, ops[0].GetFile(), src.Inline.Dir, "/test")

			t.Run("with files", func(t *testing.T) {
				src.Inline.Dir.Files = testFiles()
				ops := getSourceOp(ctx, t, src)
				checkMkdir(t, ops[0].GetFile(), src.Inline.Dir, "/test")

				if len(ops) != len(src.Inline.Dir.Files)+1 {
					t.Fatalf("expected %d ops, got %d\n%s", len(src.Inline.Dir.Files)+1, len(ops), ops)
				}

				sorted := SortMapKeys(src.Inline.Dir.Files)
				for i, name := range sorted {
					ops := getSourceOp(ctx, t, src)
					f := src.Inline.Dir.Files[name]
					checkMkfile(t, ops[i+1].GetFile(), f, name)
				}
			})
		})
	}
}

func checkMkdir(t *testing.T, op *pb.FileOp, src *SourceInlineDir, name string) {
	if op == nil {
		t.Fatal("expected dir op")
	}

	if len(op.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(op.Actions))
	}

	mkdir := op.Actions[0].GetMkdir()
	if mkdir == nil {
		t.Fatalf("expected mkdir action: %v", op.Actions[0])
	}

	if mkdir.MakeParents {
		t.Error("expected make parents to be false")
	}

	if mkdir.GetOwner().User.GetByID() != uint32(src.UID) {
		t.Errorf("expected uid %d, got %d", src.UID, mkdir.GetOwner().User.GetByID())
	}

	if mkdir.GetOwner().Group.GetByID() != uint32(src.GID) {
		t.Errorf("expected gid %d, got %d", src.GID, mkdir.GetOwner().Group.GetByID())
	}

	xPerms := src.Permissions
	if xPerms == 0 {
		xPerms = defaultDirPerms
	}
	if os.FileMode(mkdir.Mode) != xPerms {
		t.Errorf("expected mode %O, got %O", xPerms, os.FileMode(mkdir.Mode))
	}
	if mkdir.Path != "/" {
		t.Errorf("expected path %q, got %q", "/", mkdir.Path)
	}
}

func checkMkfile(t *testing.T, op *pb.FileOp, src *SourceInlineFile, name string) {
	if op == nil {
		t.Fatal("expected file op")
	}

	if len(op.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(op.Actions))
	}

	mkfile := op.Actions[0].GetMkfile()
	if mkfile == nil {
		t.Fatalf("expected mkfile action: %v", op.Actions[0])
	}

	uid := mkfile.Owner.User.GetByID()
	if uid != uint32(src.UID) {
		t.Errorf("expected uid %d, got %d", src.UID, uid)
	}

	gid := mkfile.Owner.Group.GetByID()
	if gid != uint32(src.GID) {
		t.Errorf("expected gid %d, got %d", src.GID, gid)
	}

	mode := os.FileMode(mkfile.Mode).Perm()
	xMode := src.Permissions
	if xMode == 0 {
		xMode = defaultFilePerms
	}
	if mode != xMode {
		t.Errorf("expected mode %O, got %O", xMode, mode)
	}

	if string(mkfile.Data) != src.Contents {
		t.Errorf("expected data %q, got %q", src.Contents, mkfile.Data)
	}

	xPath := filepath.Join("/", name)
	if mkfile.Path != xPath {
		t.Errorf("expected path %q, got %q", xPath, mkfile.Path)
	}
}

func stubListener(t *testing.T) net.Addr {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = l.Close()
	})

	go func() {
		defer l.Close()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}

			conn.Close()
		}
	}()

	return l.Addr()
}

// 1. Generates the LLB for a source using Source2LLBGetter (the function we are testing)
// 2. Marshals the LLB to a protobuf (since we don't have access to the data in LLB directly)
// 3. Unmarshals the protobuf to get the [pb.Op]s which is what buildkit would act on to get the actual source data during build.
func getSourceOp(ctx context.Context, t *testing.T, src Source) []*pb.Op {
	t.Helper()

	fillDefaults(&src)

	spec := &Spec{
		Sources: map[string]Source{
			"test": src,
		},
	}
	getSource := Source2LLBGetter(spec, src, "test")

	var sOpt SourceOpts
	if src.Build != nil {
		if src.Build.Inline == "" {
			t.Fatal("Cannot test from a Dockerfile without inline content")
		}
		sOpt.Forward = func(_ llb.State, build *SourceBuild) (llb.State, error) {
			// Note, we can't really test anything other than inline here because we don't have access to the actual buildkit client,
			// so we can't extract extract the dockerfile from the input state (nor do we have any input state)
			src := []byte(build.Inline)

			st, _, _, err := dockerfile2llb.Dockerfile2LLB(ctx, src, dockerfile2llb.ConvertOpt{
				MetaResolver: stubMetaResolver{},
			})
			return *st, err
		}
	}

	if src.Context != nil {
		// Note: We use this `GetContext` function (normally) to abstract away things like dockerignore and other things that docker clients tend to expect.
		// None of that makes any sense here, so we just use the normal llb.Local call.
		sOpt.GetContext = func(name string, opts ...llb.LocalOption) (*llb.State, error) {
			st := llb.Local(name, opts...)
			return &st, nil
		}
	}

	st, err := getSource(sOpt)
	if err != nil {
		t.Fatal(err)
	}

	def, err := st.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}

	out := make([]*pb.Op, 0, len(def.Def)-1)

	// We'll drop the last op which is a "return" op, which doesn't matter for our tests.
	for _, dt := range def.Def[:len(def.Def)-1] {
		op := &pb.Op{}
		if err := op.Unmarshal(dt); err != nil {
			t.Fatal(err)
		}

		out = append(out, op)
	}

	return out
}

func checkGitOp(t *testing.T, ops []*pb.Op, src *Source) {
	op := ops[0].GetSource()

	var bkAddr string

	_, other, ok := strings.Cut(src.Git.URL, "@")
	if ok {
		// ssh
		// buildkit replaces the `:` between host and port with a `/` in the identifier
		bkAddr = "git://" + strings.Replace(other, ":", "/", 1)
	} else {
		// not ssh
		_, other, ok := strings.Cut(src.Git.URL, "://")
		if !ok {
			t.Fatal("invalid git URL")
		}
		bkAddr = "git://" + other
	}

	xID := bkAddr + "#" + src.Git.Commit
	if op.Identifier != xID {
		t.Errorf("expected identifier %q, got %q", xID, op.Identifier)
	}

	if op.Attrs["git.fullurl"] != src.Git.URL {
		t.Errorf("expected git.fullurl %q, got %q", src.Git.URL, op.Attrs["git.fullurl"])
	}
}

func checkFilter(t *testing.T, op *pb.FileOp, src *Source) {
	if op == nil {
		t.Fatal("expected file op")
	}

	cpAction := op.Actions[0].GetCopy()
	if cpAction == nil {
		t.Fatal("expected copy action")
	}

	if cpAction.Dest != "/" {
		t.Errorf("expected dest \"/\", got %q", cpAction.Dest)
	}

	p := src.Path
	if src.DockerImage != nil {
		// DockerImage handles subpaths itself
		p = "/"
	}
	if !filepath.IsAbs(p) {
		p = "/" + p
	}
	if cpAction.Src != p {
		t.Errorf("expected src %q, got %q", p, cpAction.Src)
	}
	if !cpAction.DirCopyContents {
		t.Error("expected dir copy contents")
	}

	if !reflect.DeepEqual(cpAction.IncludePatterns, src.Includes) {
		t.Fatalf("expected include patterns %v, got %v", src.Includes, cpAction.IncludePatterns)
	}

	if !reflect.DeepEqual(cpAction.ExcludePatterns, src.Excludes) {
		t.Fatalf("expected exclude patterns %v, got %v", src.Excludes, cpAction.ExcludePatterns)
	}
}

func checkCmd(t *testing.T, ops []*pb.Op, src *Source) {
	if len(ops) != len(src.DockerImage.Cmd.Steps) {
		t.Fatalf("unexpected number of ops, expected %d, got %d", len(src.DockerImage.Cmd.Steps), len(ops))
	}
	for i, step := range src.DockerImage.Cmd.Steps {
		exec := ops[i].GetExec()
		if exec == nil {
			t.Error("expected exec op")
			continue
		}

		xArgs := append([]string{"/bin/sh", "-c"}, step.Command)
		if !reflect.DeepEqual(exec.Meta.Args, xArgs) {
			t.Errorf("expected args %v, got %v", xArgs, exec.Meta.Args)
		}

		xEnv := append(envMapToSlice(src.DockerImage.Cmd.Env), envMapToSlice(step.Env)...)
		slices.Sort(xEnv)
		if !reflect.DeepEqual(exec.Meta.Env, xEnv) {
			t.Errorf("expected env %v, got %v", xEnv, exec.Meta.Env)
		}

		xCwd := src.DockerImage.Cmd.Dir
		if exec.Meta.Cwd != xCwd {
			t.Errorf("expected cwd %q, got %q", xCwd, exec.Meta.Cwd)
		}

		if src.Path == "" {
			continue
		}

		// When a subpath is used, we expect a mount to be applied.
		// There should be 2 mounts, one for the rootfs and one for our subdir
		// We only care to check the 2nd mount.
		mnt := exec.Mounts[1]
		if mnt.MountType != pb.MountType_BIND {
			t.Errorf("expected bind mount, got %v", mnt.MountType)
		}
		if mnt.Dest != src.Path {
			t.Errorf("expected dest %q, got %q", src.Path, mnt.Dest)
		}
	}
}

func checkContext(t *testing.T, op *pb.SourceOp, src *Source) {
	name := src.Context.Name
	if name == "" {
		name = "context"
	}
	xID := "local://" + name
	if op.Identifier != xID {
		t.Errorf("expected identifier %q, got %q", xID, op.Identifier)
	}
}

func envMapToSlice(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, k+"="+v)
	}

	return out
}

type stubMetaResolver struct{}

func (stubMetaResolver) ResolveImageConfig(ctx context.Context, ref string, opts llb.ResolveImageConfigOpt) (string, digest.Digest, []byte, error) {
	// Craft a dummy image config
	// If we don't put at least 1 diffID, buildkit will treat this as `FROM scratch` (and actually litterally convert it `llb.Scratch`)
	// This affects what ops that get marshaled.
	// Namely it removes our `docker-image` identifier op.
	img := image.Image{
		Image: v1.Image{
			RootFS: v1.RootFS{
				DiffIDs: []digest.Digest{digest.FromBytes(nil)},
			},
		},
	}

	dt, err := json.Marshal(img)
	if err != nil {
		return "", "", nil, err
	}
	return ref, "", dt, nil
}
