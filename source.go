package dalec

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/util/gitutil"
	"github.com/pkg/errors"
)

// InvalidSourceError is an error type returned when a source is invalid.
type InvalidSourceError struct {
	Name string
	Err  error
}

func (s *InvalidSourceError) Error() string {
	return fmt.Sprintf("invalid source %s: %v", s.Name, s.Err)
}

func (s *InvalidSourceError) Unwrap() error {
	return s.Err
}

var sourceNamePathSeparatorError = errors.New("source name must not container path separator")

type LLBGetter func(sOpts SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error)

type ForwarderFunc func(llb.State, *SourceBuild) (llb.State, error)

type SourceOpts struct {
	Resolver   llb.ImageMetaResolver
	Forward    ForwarderFunc
	GetContext func(string, ...llb.LocalOption) (*llb.State, error)
}

func shArgs(cmd string) llb.RunOption {
	return llb.Args([]string{"sh", "-c", cmd})
}

// must not be called with a nil cmd pointer
func generateSourceFromImage(s *Spec, name string, st llb.State, cmd *Command, sOpts SourceOpts, subPath string, opts ...llb.ConstraintsOpt) (llb.State, error) {
	if len(cmd.Steps) == 0 {
		return llb.Scratch(), fmt.Errorf("no steps defined for image source")
	}

	for k, v := range cmd.Env {
		st = st.AddEnv(k, v)
	}
	if cmd.Dir != "" {
		st = st.Dir(cmd.Dir)
	}

	baseRunOpts := []llb.RunOption{CacheDirsToRunOpt(cmd.CacheDirs, "", "")}

	for _, src := range cmd.Mounts {
		srcSt, err := source2LLBGetter(s, src.Spec, name, true)(sOpts, opts...)
		if err != nil {
			return llb.Scratch(), err
		}
		var mountOpt []llb.MountOption
		if src.Spec.Path != "" && len(src.Spec.Includes) == 0 && len(src.Spec.Excludes) == 0 {
			mountOpt = append(mountOpt, llb.SourcePath(src.Spec.Path))
		}
		baseRunOpts = append(baseRunOpts, llb.AddMount(src.Dest, srcSt, mountOpt...))
	}

	out := llb.Scratch()
	for _, step := range cmd.Steps {
		rOpts := []llb.RunOption{llb.Args([]string{
			"/bin/sh", "-c", step.Command,
		})}

		rOpts = append(rOpts, baseRunOpts...)

		for k, v := range step.Env {
			rOpts = append(rOpts, llb.AddEnv(k, v))
		}

		rOpts = append(rOpts, withConstraints(opts))
		cmdSt := st.Run(rOpts...)
		out = cmdSt.AddMount(subPath, out)
	}

	return out, nil
}

func Source2LLBGetter(s *Spec, src Source, name string) LLBGetter {
	return source2LLBGetter(s, src, name, false)
}

// isRootPath is used to encapsulate various different possibilities for what amounts to the root path.
// It helps prevent making an extra copy of the source when it is not necessary.
func isRootPath(p string) bool {
	return p == "" || p == "/" || p == "."
}

func needsFilter(o *filterOpts) bool {
	if !isRootPath(o.source.Path) && !o.forMount && !o.pathHandled {
		return true
	}
	if o.includeExcludeHandled {
		return false
	}
	if len(o.source.Includes) > 0 || len(o.source.Excludes) > 0 {
		return true
	}
	return false
}

type filterOpts struct {
	state                 llb.State
	source                Source
	opts                  []llb.ConstraintsOpt
	forMount              bool
	includeExcludeHandled bool
	pathHandled           bool
	err                   error
}

func handleFilter(o *filterOpts) (llb.State, error) {
	if o.err != nil {
		return o.state, o.err
	}

	if !needsFilter(o) {
		return o.state, nil
	}

	srcPath := "/"
	if !o.pathHandled {
		srcPath = o.source.Path
	}

	filtered := llb.Scratch().File(
		llb.Copy(
			o.state,
			srcPath,
			"/",
			WithIncludes(o.source.Includes),
			WithExcludes(o.source.Excludes),
			WithDirContentsOnly(),
		),
		withConstraints(o.opts),
	)

	return filtered, nil
}

var errNoSourceVariant = fmt.Errorf("no source variant found")

func source2LLBGetter(s *Spec, src Source, name string, forMount bool) LLBGetter {
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (ret llb.State, retErr error) {
		var (
			includeExcludeHandled bool
			pathHandled           bool
		)

		defer func() {
			ret, retErr = handleFilter(&filterOpts{
				state:                 ret,
				source:                src,
				opts:                  opts,
				forMount:              forMount,
				includeExcludeHandled: includeExcludeHandled,
				pathHandled:           pathHandled,
				err:                   retErr,
			})
		}()

		switch {
		case src.DockerImage != nil:
			img := src.DockerImage
			st := llb.Image(img.Ref, llb.WithMetaResolver(sOpt.Resolver), withConstraints(opts))
			if img.Cmd == nil {
				return st, nil
			}

			st, err := generateSourceFromImage(s, name, st, img.Cmd, sOpt, src.Path, opts...)
			if err != nil {
				return llb.Scratch(), err
			}
			pathHandled = true
			return st, nil
		case src.Git != nil:
			url := src.Git.URL
			commit := src.Git.Commit
			// TODO: Pass git secrets
			ref, err := gitutil.ParseGitRef(url)
			if err != nil {
				return llb.Scratch(), fmt.Errorf("could not parse git ref: %w", err)
			}

			var gOpts []llb.GitOption
			if src.Git.KeepGitDir {
				gOpts = append(gOpts, llb.KeepGitDir())
			}
			gOpts = append(gOpts, withConstraints(opts))
			return llb.Git(ref.Remote, commit, gOpts...), nil
		case src.HTTP != nil:
			https := src.HTTP
			opts := []llb.HTTPOption{withConstraints(opts)}
			opts = append(opts, llb.Filename(name))
			return llb.HTTP(https.URL, opts...), nil
		case src.Context != nil:
			st, err := sOpt.GetContext(src.Context.Name, localIncludeExcludeMerge(&src))
			if err != nil {
				return llb.Scratch(), err
			}

			if st == nil {
				return llb.Scratch(), errors.Errorf("context %q not found", name)
			}

			includeExcludeHandled = true
			return *st, nil
		case src.Build != nil:
			build := src.Build

			st, err := source2LLBGetter(s, build.Source, name, forMount)(sOpt, opts...)
			if err != nil {
				if !errors.Is(err, errNoSourceVariant) || build.Inline == "" {
					return llb.Scratch(), err
				}
				st = llb.Scratch()
			}

			return sOpt.Forward(st, build)
		case src.Inline != nil:
			if src.Inline.File != nil {
				return llb.Scratch().With(src.Inline.File.PopulateAt(name)), nil
			}
			return llb.Scratch().With(src.Inline.Dir.PopulateAt("/")), nil
		default:
			return llb.Scratch(), errNoSourceVariant
		}
	}
}

func sharingMode(mode string) (llb.CacheMountSharingMode, error) {
	switch mode {
	case "shared", "":
		return llb.CacheMountShared, nil
	case "private":
		return llb.CacheMountPrivate, nil
	case "locked":
		return llb.CacheMountLocked, nil
	default:
		return 0, fmt.Errorf("invalid sharing mode: %s", mode)
	}
}

func WithCreateDestPath() llb.CopyOption {
	return copyOptionFunc(func(i *llb.CopyInfo) {
		i.CreateDestPath = true
	})
}

func SourceIsDir(src Source) (bool, error) {
	switch {
	case src.DockerImage != nil,
		src.Git != nil,
		src.Build != nil,
		src.Context != nil:
		return true, nil
	case src.HTTP != nil:
		return false, nil
	case src.Inline != nil:
		return src.Inline.Dir != nil, nil
	default:
		return false, fmt.Errorf("unsupported source type")
	}
}

// Doc returns the details of how the source was created.
// This should be included, where applicable, in build in build specs (such as RPM spec files)
// so that others can reproduce the build.
func (s Source) Doc(name string) (io.Reader, error) {
	b := bytes.NewBuffer(nil)
	switch {
	case s.Context != nil:
		fmt.Fprintln(b, "Generated from a local docker build context and is unreproducible.")
	case s.Build != nil:
		fmt.Fprintln(b, "Generated from a docker build:")
		fmt.Fprintln(b, "	Docker Build Target:", s.Build.Target)
		sub, err := s.Build.Source.Doc(name)
		if err != nil {
			return nil, err
		}

		scanner := bufio.NewScanner(sub)
		for scanner.Scan() {
			fmt.Fprintf(b, "			%s\n", scanner.Text())
		}
		if scanner.Err() != nil {
			return nil, scanner.Err()
		}

		if len(s.Build.Args) > 0 {
			sorted := SortMapKeys(s.Build.Args)
			fmt.Fprintln(b, "	Build Args:")
			for _, k := range sorted {
				fmt.Fprintf(b, "		%s=%s\n", k, s.Build.Args[k])
			}
		}

		switch {
		case s.Build.Inline != "":
			fmt.Fprintln(b, "	Dockerfile:")

			scanner := bufio.NewScanner(strings.NewReader(s.Build.Inline))
			for scanner.Scan() {
				fmt.Fprintf(b, "		%s\n", scanner.Text())
			}
			if scanner.Err() != nil {
				return nil, scanner.Err()
			}
		default:
			p := "Dockerfile"
			if s.Build.DockerFile != "" {
				p = s.Build.DockerFile
			}
			fmt.Fprintln(b, "	Dockerfile path in context:", p)
		}
	case s.HTTP != nil:
		fmt.Fprintln(b, "Generated from a http(s) source:")
		fmt.Fprintln(b, "	URL:", s.HTTP.URL)
	case s.Git != nil:
		git := s.Git
		ref, err := gitutil.ParseGitRef(git.URL)
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(b, "Generated from a git repository:")
		fmt.Fprintln(b, "	Remote:", ref.Remote)
		fmt.Fprintln(b, "	Ref:", git.Commit)
		if s.Path != "" {
			fmt.Fprintln(b, "	Extraced path:", s.Path)
		}
	case s.DockerImage != nil:
		img := s.DockerImage
		if img.Cmd == nil {
			fmt.Fprintln(b, "Generated from a docker image:")
			fmt.Fprintln(b, "	Image:", img.Ref)
			if s.Path != "" {
				fmt.Fprintln(b, "	Extraced path:", s.Path)
			}
		} else {
			fmt.Fprintln(b, "Generated from running a command(s) in a docker image:")
			fmt.Fprintln(b, "	Image:", img.Ref)
			if s.Path != "" {
				fmt.Fprintln(b, "	Extraced path:", s.Path)
			}
			if len(img.Cmd.Env) > 0 {
				fmt.Fprintln(b, "	With the following environment variables set for all commands:")

				sorted := SortMapKeys(img.Cmd.Env)
				for _, k := range sorted {
					fmt.Fprintf(b, "		%s=%s\n", k, img.Cmd.Env[k])
				}
			}
			if img.Cmd.Dir != "" {
				fmt.Fprintln(b, "	Working Directory:", img.Cmd.Dir)
			}
			fmt.Fprintln(b, "	Command(s):")
			for _, step := range img.Cmd.Steps {
				fmt.Fprintf(b, "		%s\n", step.Command)
				if len(step.Env) > 0 {
					fmt.Fprintln(b, "			With the following environment variables set for this command:")
					sorted := SortMapKeys(step.Env)
					for _, k := range sorted {
						fmt.Fprintf(b, "				%s=%s\n", k, step.Env[k])
					}
				}
			}
			if len(img.Cmd.Mounts) > 0 {
				fmt.Fprintln(b, "	With the following items mounted:")
				for _, src := range img.Cmd.Mounts {
					sub, err := src.Spec.Doc(name)
					if err != nil {
						return nil, err
					}

					fmt.Fprintln(b, "		Destination Path:", src.Dest)
					scanner := bufio.NewScanner(sub)
					for scanner.Scan() {
						fmt.Fprintf(b, "			%s\n", scanner.Text())
					}
					if scanner.Err() != nil {
						return nil, scanner.Err()
					}
				}
			}
			return b, nil
		}
	case s.Inline != nil:
		fmt.Fprintln(b, "Generated from an inline source:")
		s.Inline.Doc(b, name)
	default:
		// This should be unrecable.
		// We could panic here, but ultimately this is just a doc string and parsing user generated content.
		fmt.Fprintln(b, "Generated from an unknown source type")
	}

	return b, nil
}

func patchSource(worker, sourceState llb.State, sourceToState map[string]llb.State, patchNames []PatchSpec, opts ...llb.ConstraintsOpt) llb.State {
	for _, p := range patchNames {
		patchState := sourceToState[p.Source]
		// on each iteration, mount source state to /src to run `patch`, and
		// set the state under /src to be the source state for the next iteration
		sourceState = worker.Run(
			llb.AddMount("/patch", patchState, llb.Readonly, llb.SourcePath(p.Source)),
			llb.Dir("src"),
			shArgs(fmt.Sprintf("patch -p%d < /patch", *p.Strip)),
			WithConstraints(opts...),
		).AddMount("/src", sourceState)
	}

	return sourceState
}

// `sourceToState` must be a complete map from source name -> llb state for each source in the dalec spec.
// `worker` must be an LLB state with a `patch` binary present.
// PatchSources returns a new map containing the patched LLB state for each source in the source map.
func PatchSources(worker llb.State, spec *Spec, sourceToState map[string]llb.State, opts ...llb.ConstraintsOpt) map[string]llb.State {
	// duplicate map to avoid possibly confusing behavior of mutating caller's map
	states := DuplicateMap(sourceToState)
	sorted := SortMapKeys(spec.Sources)

	for _, sourceName := range sorted {
		sourceState := states[sourceName]

		patches, patchesExist := spec.Patches[sourceName]
		if !patchesExist {
			continue
		}
		opts = append(opts, ProgressGroup("Patch spec source:"+sourceName))
		states[sourceName] = patchSource(worker, sourceState, states, patches, withConstraints(opts))
	}

	return states
}
