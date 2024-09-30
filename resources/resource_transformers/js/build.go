// Copyright 2020 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package js

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/afero"

	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/hugofs"
	"github.com/gohugoio/hugo/media"

	"github.com/gohugoio/hugo/common/herrors"
	"github.com/gohugoio/hugo/common/text"

	"github.com/gohugoio/hugo/hugolib/filesystems"
	"github.com/gohugoio/hugo/resources/internal"
	"github.com/gohugoio/hugo/resources/resource"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/gohugoio/hugo/resources"
)

// Client context for ESBuild.
type Client struct {
	rs  *resources.Spec
	sfs *filesystems.SourceFilesystem
}

// New creates a new client context.
func New(fs *filesystems.SourceFilesystem, rs *resources.Spec) *Client {
	return &Client{
		rs:  rs,
		sfs: fs,
	}
}

func (c *Client) Transform(optsm map[string]any, r []resources.ResourceTransformer, single bool) (resource.Resources, error) {
	if len(r) == 0 {
		return nil, nil
	}

	opts, err := decodeOptions(optsm)
	if err != nil {
		return nil, err
	}

	opts.resolveDir = c.rs.Cfg.BaseConfig().WorkingDir // where node_modules gets resolved
	opts.tsConfig = c.rs.ResolveJSConfigFile("tsconfig.json")

	buildOptions, err := toBuildOptions(opts)
	if err != nil {
		return nil, err
	}

	buildOptions.Outdir, err = os.MkdirTemp(os.TempDir(), "compileOutput")
	if err != nil {
		return nil, err
	}
	defer os.Remove(buildOptions.Outdir)

	if opts.Inject != nil {
		// Resolve the absolute filenames.
		for i, ext := range opts.Inject {
			impPath := filepath.FromSlash(ext)
			if filepath.IsAbs(impPath) {
				return nil, fmt.Errorf("inject: absolute paths not supported, must be relative to /assets")
			}

			m := resolveComponentInAssets(c.rs.Assets.Fs, impPath)

			if m == nil {
				return nil, fmt.Errorf("inject: file %q not found", ext)
			}

			opts.Inject[i] = m.Filename

		}

		buildOptions.Inject = opts.Inject
	}

	buildOptions.EntryPoints = make([]string, 0, len(r))
	for _, ext := range r {
		impPath := filepath.FromSlash(ext.Name())
		m := resolveComponentInAssets(c.rs.Assets.Fs, impPath)
		if m == nil {
			return nil, fmt.Errorf("file %q not found", ext.Name())
		}

		buildOptions.EntryPoints = append(buildOptions.EntryPoints, m.Filename)
	}

	result := api.Build(buildOptions)

	if len(result.Errors) > 0 {
		createErr := func(msg api.Message) error {
			loc := msg.Location
			if loc == nil {
				return errors.New(msg.Text)
			}
			path := loc.File

			errorMessage := msg.Text
			errorMessage = strings.ReplaceAll(errorMessage, nsImportHugo+":", "")

			var (
				f   afero.File
				err error
			)

			if strings.HasPrefix(path, nsImportHugo) {
				path = strings.TrimPrefix(path, nsImportHugo+":")
				f, err = hugofs.Os.Open(path)
			} else {
				var fi os.FileInfo
				fi, err = c.sfs.Fs.Stat(path)
				if err == nil {
					m := fi.(hugofs.FileMetaInfo).Meta()
					path = m.Filename
					f, err = m.Open()
				}

			}

			if err == nil {
				fe := herrors.
					NewFileErrorFromName(errors.New(errorMessage), path).
					UpdatePosition(text.Position{Offset: -1, LineNumber: loc.Line, ColumnNumber: loc.Column}).
					UpdateContent(f, nil)

				f.Close()
				return fe
			}

			return fmt.Errorf("%s", errorMessage)
		}

		var errors []error

		for _, msg := range result.Errors {
			errors = append(errors, createErr(msg))
		}

		// Return 1, log the rest.
		for i, err := range errors {
			if i > 0 {
				c.rs.Logger.Errorf("js.Build failed: %s", err)
			}
		}

		return nil, errors[0]
	}

	entryPointsMap := make(map[string]resources.ResourceTransformer, len(buildOptions.EntryPoints)*2)
	outBase := lowestCommonAncestorDirectory(buildOptions.EntryPoints)

	// we need to know the full paths of the entry points in the output
	for _, ext := range r {
		impPath := filepath.FromSlash(ext.Name())
		m := resolveComponentInAssets(c.rs.Assets.Fs, impPath)
		if m == nil {
			return nil, fmt.Errorf("file %q not found", ext.Name())
		}

		// remove starting common path
		name := strings.TrimPrefix(m.Filename, outBase)

		// remove extension
		name = strings.TrimSuffix(name, filepath.Ext(name))

		// add tmp dir prefix
		name = filepath.Join(buildOptions.Outdir, name)
		nameJS := name + ".js"

		// add entry point to map
		entryPointsMap[nameJS] = ext
		if !single {
			entryPointsMap[name+".css"] = ext
		}
	}

	type entryPoint struct {
		r resources.ResourceTransformer
		f api.OutputFile
	}

	var (
		// entryPointFiles are the files that we expect to be referencing,
		// such as the literal js/ts file we gave it, or the matching css file.
		entryPointFiles []entryPoint

		// addlFiles is the source maps and chunks that are created, things that we aren't
		// expecting to manually publish, but that just need to be there for importing or debugging.
		addlFiles []api.OutputFile
	)

	outDir := opts.TargetPath
	if single {
		outDir = filepath.Dir(opts.TargetPath)
	}

	for _, f := range result.OutputFiles {
		realPath := f.Path

		path := strings.TrimPrefix(f.Path, buildOptions.Outdir)
		path = filepath.Join(outDir, path)
		f.Path = path

		if r, ok := entryPointsMap[realPath]; ok {
			entryPointFiles = append(entryPointFiles, entryPoint{r: r, f: f})
		} else {
			addlFiles = append(addlFiles, f)
		}
	}

	if len(entryPointFiles) == 0 {
		return nil, nil
	}

	res := make(resource.Resources, 0, len(entryPointFiles))

	for _, f := range entryPointFiles {
		var mediaType media.Type
		switch filepath.Ext(f.f.Path) {
		case ".js":
			mediaType = media.Builtin.JavascriptType
		case ".css":
			mediaType = media.Builtin.CSSType
		}

		path := f.f.Path
		if single {
			path = opts.TargetPath
		}

		t, err := f.r.Transform(&entrypointTransformation{
			optsm: map[string]any{
				"contents":   string(f.f.Contents),
				"mediaType":  mediaType,
				"targetPath": path,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to transform resource: %w", err)
		}

		res = append(res, t)
	}

	var addlFilesBase string
	if single && opts.TargetPath == "" {
		addlFilesBase = strings.TrimPrefix(filepath.Dir(r[0].Name()), "/")
	}

	for _, f := range addlFiles {
		path := filepath.Join(addlFilesBase, f.Path)
		if err = c.Publish(path, string(f.Contents)); err != nil {
			return nil, err
		}
	}

	return res, nil
}

type entrypointTransformation struct {
	optsm map[string]any
}

func (t *entrypointTransformation) Key() internal.ResourceTransformationKey {
	return internal.NewResourceTransformationKey("jsbuild", t.optsm)
}

func (t *entrypointTransformation) Transform(ctx *resources.ResourceTransformationCtx) error {
	var opts struct {
		Contents   string
		MediaType  media.Type
		TargetPath string
	}

	if err := mapstructure.WeakDecode(t.optsm, &opts); err != nil {
		return err
	}

	_, err := ctx.To.Write([]byte(opts.Contents))
	if err != nil {
		return err
	}

	ctx.OutMediaType = opts.MediaType
	ctx.OutPath = opts.TargetPath

	return nil
}

func (c *Client) Publish(target, content string) error {
	f, err := helpers.OpenFilesForWriting(c.rs.BaseFs.PublishFs, target)
	if err != nil {
		return fmt.Errorf("failed to open file for publishing %q: %w", target, err)
	}

	defer f.Close()
	_, err = f.Write([]byte(content))
	return err
}

// Process process esbuild transform
func (c *Client) Process(maybeRes any, opts map[string]any) (any, error) {
	var single bool
	var res []resources.ResourceTransformer
	switch r := maybeRes.(type) {
	case resources.ResourceTransformer:
		res = []resources.ResourceTransformer{r}
		single = true
	case []resources.ResourceTransformer:
		res = r
	default:
		return nil, fmt.Errorf("type %T not supported in Resource transformations", maybeRes)
	}

	names := make([]string, 0, len(res))
	for _, r := range res {
		names = append(names, r.Name())
	}

	key := internal.NewResourceTransformationKey("jsbuild", names, opts)

	r, err := c.rs.ResourceCache.GetOrCreateResources(key.Value(), func() (resource.Resources, error) {
		return c.Transform(opts, res, single)
	})
	if err != nil {
		return nil, err
	}

	if single {
		return r[0], nil
	}

	return &resources.ResourceCollection{Resources: r, BasePath: c.rs.PathSpec.GetBasePath(false)}, nil
}

// lowestCommonAncestorDirectory returns the lowest common directory of the given entry points.
// See https://github.com/evanw/esbuild/blob/d34e79e2a998c21bb71d57b92b0017ca11756912/internal/bundler/bundler.go#L1957
func lowestCommonAncestorDirectory(paths []string) string {
	if len(paths) == 0 {
		return ""
	}

	lowestAbsDir := filepath.Dir(paths[0])

	for _, path := range paths[1:] {
		absDir := filepath.Dir(path)
		lastSlash := 0
		a := 0
		b := 0

		for {
			runeA, widthA := utf8.DecodeRuneInString(absDir[a:])
			runeB, widthB := utf8.DecodeRuneInString(lowestAbsDir[b:])
			boundaryA := widthA == 0 || runeA == '/' || runeA == '\\'
			boundaryB := widthB == 0 || runeB == '/' || runeB == '\\'

			if boundaryA && boundaryB {
				if widthA == 0 || widthB == 0 {
					// Truncate to the smaller path if one path is a prefix of the other
					lowestAbsDir = absDir[:a]
					break
				} else {
					// Track the longest common directory so far
					lastSlash = a
				}
			} else if boundaryA != boundaryB || unicode.ToLower(runeA) != unicode.ToLower(runeB) {
				// If we're at the top-level directory, then keep the slash
				if lastSlash < len(absDir) && !strings.ContainsAny(absDir[:lastSlash], "\\/") {
					lastSlash++
				}

				// If both paths are different at this point, stop and set the lowest so
				// far to the common parent directory. Compare using a case-insensitive
				// comparison to handle paths on Windows.
				lowestAbsDir = absDir[:lastSlash]
				break
			}

			a += widthA
			b += widthB
		}
	}

	return lowestAbsDir
}
