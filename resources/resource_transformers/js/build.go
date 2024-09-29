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
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/afero"

	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/hugofs"
	"github.com/gohugoio/hugo/media"

	"github.com/gohugoio/hugo/common/herrors"
	"github.com/gohugoio/hugo/common/text"

	"github.com/gohugoio/hugo/hugolib/filesystems"
	"github.com/gohugoio/hugo/resources/internal"

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

type buildTransformation struct {
	optsm map[string]any
	c     *Client
}

func (t *buildTransformation) Key() internal.ResourceTransformationKey {
	return internal.NewResourceTransformationKey("jsbuild", t.optsm)
}

func (t *buildTransformation) Transform(ctx *resources.ResourceTransformationCtx) error {
	ctx.OutMediaType = media.Builtin.JavascriptType

	opts, err := decodeOptions(t.optsm)
	if err != nil {
		return err
	}

	if opts.TargetPath != "" {
		ctx.OutPath = opts.TargetPath
	} else {
		ctx.ReplaceOutPathExtension(".js")
	}

	src, err := io.ReadAll(ctx.From)
	if err != nil {
		return err
	}

	opts.sourceDir = filepath.FromSlash(path.Dir(ctx.SourcePath))
	opts.resolveDir = t.c.rs.Cfg.BaseConfig().WorkingDir // where node_modules gets resolved
	opts.contents = string(src)
	opts.tsConfig = t.c.rs.ResolveJSConfigFile("tsconfig.json")

	buildOptions, err := toBuildOptions(opts)
	if err != nil {
		return err
	}

	buildOptions.Plugins, err = createBuildPlugins(ctx.DependencyManager, t.c, opts)
	if err != nil {
		return err
	}

	buildOptions.Outdir, err = os.MkdirTemp(os.TempDir(), "compileOutput")
	if err != nil {
		return err
	}
	defer os.Remove(buildOptions.Outdir)

	if opts.Inject != nil {
		// Resolve the absolute filenames.
		for i, ext := range opts.Inject {
			impPath := filepath.FromSlash(ext)
			if filepath.IsAbs(impPath) {
				return fmt.Errorf("inject: absolute paths not supported, must be relative to /assets")
			}

			m := resolveComponentInAssets(t.c.rs.Assets.Fs, impPath)

			if m == nil {
				return fmt.Errorf("inject: file %q not found", ext)
			}

			opts.Inject[i] = m.Filename

		}

		buildOptions.Inject = opts.Inject

	}

	if len(buildOptions.EntryPoints) == 0 {
		buildOptions.EntryPoints = []string{ctx.SourcePath}
	}

	// Resolve the absolute filenames.
	for i, ext := range buildOptions.EntryPoints {
		impPath := filepath.FromSlash(ext)
		if filepath.IsAbs(impPath) {
			return fmt.Errorf("entryPoints: absolute paths not supported, must be relative to /assets")
		}

		m := resolveComponentInAssets(t.c.rs.Assets.Fs, impPath)

		if m == nil {
			return fmt.Errorf("entryPoints: file %q not found", ext)
		}

		buildOptions.EntryPoints[i] = m.Filename
	}

	result := api.Build(buildOptions)

	if len(result.Errors) > 0 {

		createErr := func(msg api.Message) error {
			loc := msg.Location
			if loc == nil {
				return errors.New(msg.Text)
			}
			path := loc.File
			if path == stdinImporter {
				path = ctx.SourcePath
			}

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
				fi, err = t.c.sfs.Fs.Stat(path)
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
				t.c.rs.Logger.Errorf("js.Build failed: %s", err)
			}
		}

		return errors[0]
	}

	var (
		// even though we may have multiple output files, we need to get the outfile that corresponds
		// with the first entry point so we can return its contents for the resulting resource
		outFile    string
		outputFile api.OutputFile
	)

	// build a list of all entrypoints in the output dir so we can differentiate between entry points and additionally created chunks
	entryPointsMap := make(map[string]struct{}, len(buildOptions.EntryPoints))
	outBase := lowestCommonAncestorDirectory(buildOptions.EntryPoints)

	// we need to know the full paths of the entry points in the output
	for i, e := range buildOptions.EntryPoints {
		// remove starting common path
		name := strings.TrimPrefix(e, outBase)

		// remove extension, replace with ".js" (js.Build doesn't support CSS so this is the only option)
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ".js"

		// add tmp dir prefix
		name = filepath.Join(buildOptions.Outdir, name)

		// add entry point to map
		entryPointsMap[name] = struct{}{}

		if i == 0 {
			outFile = name
		}
	}

	for _, f := range result.OutputFiles {
		if f.Path == outFile {
			outputFile = f
			break
		}
	}

	outDir := filepath.Dir(ctx.OutPath)

	// "Publish" the additional files that were created so the imports & sourcemaps work
	for _, f := range result.OutputFiles {
		path := strings.TrimPrefix(f.Path, buildOptions.Outdir)
		path = filepath.Join(outDir, path)

		// if _, ok := entryPointsMap[f.Path]; ok {
		// 	TODO: mount to assets somehow?
		// }

		if err = ctx.Publish(path, string(f.Contents)); err != nil {
			return err
		}
	}

	if buildOptions.Sourcemap == api.SourceMapExternal {
		// add it to the output file to keep "external" working as originally intended
		content := string(outputFile.Contents)
		symPath := path.Base(ctx.OutPath) + ".map"
		content += "\n//# sourceMappingURL=" + symPath + "\n"

		for _, f := range result.OutputFiles {
			if f.Path == outFile+".map" {
				if err = ctx.PublishSourceMap(string(f.Contents)); err != nil {
					return err
				}
				break
			}
		}
		_, err := ctx.To.Write([]byte(content))
		if err != nil {
			return err
		}
	} else if buildOptions.Sourcemap == api.SourceMapLinked {
		content := string(outputFile.Contents)
		symPath := path.Base(ctx.OutPath) + ".map"
		re := regexp.MustCompile(`//# sourceMappingURL=.*\n?`)
		content = re.ReplaceAllString(content, "//# sourceMappingURL="+symPath+"\n")

		for _, f := range result.OutputFiles {
			if f.Path == outFile+".map" {
				if err = ctx.PublishSourceMap(string(f.Contents)); err != nil {
					return err
				}
				break
			}
		}
		_, err := ctx.To.Write([]byte(content))
		if err != nil {
			return err
		}
	} else {
		_, err := ctx.To.Write(outputFile.Contents)
		if err != nil {
			return err
		}
	}

	return nil
}

func transform(t *buildTransformation, r []resources.ResourceTransformer) ([]resources.ResourceTransformer, error) {
	opts, err := decodeOptions(t.optsm)
	if err != nil {
		return nil, err
	}

	opts.resolveDir = t.c.rs.Cfg.BaseConfig().WorkingDir // where node_modules gets resolved
	opts.tsConfig = t.c.rs.ResolveJSConfigFile("tsconfig.json")

	buildOptions, err := toBuildOptions(opts)
	if err != nil {
		return nil, err
	}

	buildOptions.Outdir, err = os.MkdirTemp(os.TempDir(), "compileOutput")
	if err != nil {
		return nil, err
	}
	defer os.Remove(buildOptions.Outdir)

	buildOptions.EntryPoints = make([]string, 0, len(r))
	for _, ext := range r {
		impPath := filepath.FromSlash(ext.Name())
		m := resolveComponentInAssets(t.c.rs.Assets.Fs, impPath)
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
				fi, err = t.c.sfs.Fs.Stat(path)
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
				t.c.rs.Logger.Errorf("js.Build failed: %s", err)
			}
		}

		return nil, errors[0]
	}

	entryPointsMap := make(map[string]resources.ResourceTransformer, len(buildOptions.EntryPoints)*2)
	outBase := lowestCommonAncestorDirectory(buildOptions.EntryPoints)

	// we need to know the full paths of the entry points in the output
	for _, ext := range r {
		impPath := filepath.FromSlash(ext.Name())
		m := resolveComponentInAssets(t.c.rs.Assets.Fs, impPath)
		if m == nil {
			return nil, fmt.Errorf("file %q not found", ext.Name())
		}

		// remove starting common path
		name := strings.TrimPrefix(m.Filename, outBase)

		// remove extensio
		name = strings.TrimSuffix(name, filepath.Ext(name))

		// add tmp dir prefix
		name = filepath.Join(buildOptions.Outdir, name)
		nameJS := name + ".js"

		// add entry point to map
		entryPointsMap[nameJS] = ext
		entryPointsMap[name+".css"] = ext
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

	res := make([]resources.ResourceTransformer, 0, len(entryPointFiles))

	for _, f := range entryPointFiles {
		var mediaType media.Type
		switch filepath.Ext(f.f.Path) {
		case ".js":
			mediaType = media.Builtin.JavascriptType
		case ".css":
			mediaType = media.Builtin.CSSType
		}

		t, err := f.r.Transform(&outTransformation{
			optsm: map[string]any{
				"contents":   string(f.f.Contents),
				"mediaType":  mediaType,
				"targetPath": f.f.Path,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to transform resource: %w", err)
		}

		res = append(res, t)
	}

	for _, f := range addlFiles {
		if err = publish(t, f.Path, string(f.Contents)); err != nil {
			return nil, err
		}
	}

	return res, nil
}

type outTransformation struct {
	optsm map[string]any
}

func (t *outTransformation) Key() internal.ResourceTransformationKey {
	return internal.NewResourceTransformationKey("jsbuild", t.optsm)
}

func (t *outTransformation) Transform(ctx *resources.ResourceTransformationCtx) error {
	_, err := ctx.To.Write([]byte(t.optsm["contents"].(string)))
	if err != nil {
		return err
	}

	ctx.OutMediaType = t.optsm["mediaType"].(media.Type)
	ctx.OutPath = t.optsm["targetPath"].(string)

	return nil
}

func publish(t *buildTransformation, target, content string) error {
	f, err := helpers.OpenFilesForWriting(t.c.rs.BaseFs.PublishFs, target)
	if err != nil {
		return fmt.Errorf("failed to open file for publishing %q: %w", target, err)
	}

	defer f.Close()
	_, err = f.Write([]byte(content))
	return err
}

// Process process esbuild transform
func (c *Client) Process(res any, opts map[string]any) (any, error) {
	t := &buildTransformation{c: c, optsm: opts}

	switch r := res.(type) {
	case resources.ResourceTransformer:
		return r.Transform(t)
	case []resources.ResourceTransformer:
		return transform(t, r)
	default:
		return nil, fmt.Errorf("type %T not supported in Resource transformations", res)
	}
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
