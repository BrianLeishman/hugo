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
	"path"
	"path/filepath"
	"testing"

	"github.com/gohugoio/hugo/config"
	"github.com/gohugoio/hugo/config/testconfig"
	"github.com/gohugoio/hugo/hugofs"
	"github.com/gohugoio/hugo/hugolib/filesystems"
	"github.com/gohugoio/hugo/hugolib/paths"

	"github.com/spf13/afero"

	"github.com/evanw/esbuild/pkg/api"

	qt "github.com/frankban/quicktest"
)

// This test is added to test/warn against breaking the "stability" of the
// cache key. It's sometimes needed to break this, but should be avoided if possible.
func TestOptionKey(t *testing.T) {
	c := qt.New(t)

	opts := map[string]any{
		"TargetPath": "foo",
		"Target":     "es2018",
	}

	key := (&buildTransformation{optsm: opts}).Key()

	c.Assert(key.Value(), qt.Equals, "jsbuild_1533819657654811600")
}

func TestToBuildOptions(t *testing.T) {
	c := qt.New(t)

	opts, err := toBuildOptions(Options{})

	c.Assert(err, qt.IsNil)
	c.Assert(opts, qt.DeepEquals, api.BuildOptions{
		Bundle: true,
		Target: api.ESNext,
		Format: api.FormatIIFE,
	})

	opts, err = toBuildOptions(Options{
		Target:   "es2018",
		Format:   "cjs",
		Minify:   true,
		AvoidTDZ: true,
	})
	c.Assert(err, qt.IsNil)
	c.Assert(opts, qt.DeepEquals, api.BuildOptions{
		Bundle:            true,
		Target:            api.ES2018,
		Format:            api.FormatCommonJS,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		MinifyWhitespace:  true,
	})

	opts, err = toBuildOptions(Options{
		Target: "es2018", Format: "cjs", Minify: true,
		SourceMap: "inline",
	})
	c.Assert(err, qt.IsNil)
	c.Assert(opts, qt.DeepEquals, api.BuildOptions{
		Bundle:            true,
		Target:            api.ES2018,
		Format:            api.FormatCommonJS,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		MinifyWhitespace:  true,
		Sourcemap:         api.SourceMapInline,
	})

	opts, err = toBuildOptions(Options{
		Target: "es2018", Format: "cjs", Minify: true,
		SourceMap: "inline",
	})
	c.Assert(err, qt.IsNil)
	c.Assert(opts, qt.DeepEquals, api.BuildOptions{
		Bundle:            true,
		Target:            api.ES2018,
		Format:            api.FormatCommonJS,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		MinifyWhitespace:  true,
		Sourcemap:         api.SourceMapInline,
	})

	opts, err = toBuildOptions(Options{
		Target: "es2018", Format: "cjs", Minify: true,
		SourceMap: "external",
	})
	c.Assert(err, qt.IsNil)
	c.Assert(opts, qt.DeepEquals, api.BuildOptions{
		Bundle:            true,
		Target:            api.ES2018,
		Format:            api.FormatCommonJS,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		MinifyWhitespace:  true,
		Sourcemap:         api.SourceMapExternal,
	})

	opts, err = toBuildOptions(Options{
		JSX: "automatic", JSXImportSource: "preact",
	})
	c.Assert(err, qt.IsNil)
	c.Assert(opts, qt.DeepEquals, api.BuildOptions{
		Bundle:          true,
		Target:          api.ESNext,
		Format:          api.FormatIIFE,
		JSX:             api.JSXAutomatic,
		JSXImportSource: "preact",
	})
}

func TestToBuildOptionsTarget(t *testing.T) {
	c := qt.New(t)

	for _, test := range []struct {
		target string
		expect api.Target
	}{
		{"es2015", api.ES2015},
		{"es2016", api.ES2016},
		{"es2017", api.ES2017},
		{"es2018", api.ES2018},
		{"es2019", api.ES2019},
		{"es2020", api.ES2020},
		{"es2021", api.ES2021},
		{"es2022", api.ES2022},
		{"es2023", api.ES2023},
		{"", api.ESNext},
		{"esnext", api.ESNext},
	} {
		c.Run(test.target, func(c *qt.C) {
			opts, err := toBuildOptions(Options{
				Target: test.target,
			})
			c.Assert(err, qt.IsNil)
			c.Assert(opts.Target, qt.Equals, test.expect)
		})
	}
}

func TestResolveComponentInAssets(t *testing.T) {
	c := qt.New(t)

	for _, test := range []struct {
		name    string
		files   []string
		impPath string
		expect  string
	}{
		{"Basic, extension", []string{"foo.js", "bar.js"}, "foo.js", "foo.js"},
		{"Basic, no extension", []string{"foo.js", "bar.js"}, "foo", "foo.js"},
		{"Basic, no extension, typescript", []string{"foo.ts", "bar.js"}, "foo", "foo.ts"},
		{"Not found", []string{"foo.js", "bar.js"}, "moo.js", ""},
		{"Not found, double js extension", []string{"foo.js.js", "bar.js"}, "foo.js", ""},
		{"Index file, folder only", []string{"foo/index.js", "bar.js"}, "foo", "foo/index.js"},
		{"Index file, folder and index", []string{"foo/index.js", "bar.js"}, "foo/index", "foo/index.js"},
		{"Index file, folder and index and suffix", []string{"foo/index.js", "bar.js"}, "foo/index.js", "foo/index.js"},
		{"Index ESM file, folder only", []string{"foo/index.esm.js", "bar.js"}, "foo", "foo/index.esm.js"},
		{"Index ESM file, folder and index", []string{"foo/index.esm.js", "bar.js"}, "foo/index", "foo/index.esm.js"},
		{"Index ESM file, folder and index and suffix", []string{"foo/index.esm.js", "bar.js"}, "foo/index.esm.js", "foo/index.esm.js"},
		// We added these index.esm.js cases in v0.101.0. The case below is unlikely to happen in the wild, but add a test
		// to document Hugo's behavior. We pick the file with the name index.js; anything else would be breaking.
		{"Index and Index ESM file, folder only", []string{"foo/index.esm.js", "foo/index.js", "bar.js"}, "foo", "foo/index.js"},

		// Issue #8949
		{"Check file before directory", []string{"foo.js", "foo/index.js"}, "foo", "foo.js"},
	} {
		c.Run(test.name, func(c *qt.C) {
			baseDir := "assets"
			mfs := afero.NewMemMapFs()

			for _, filename := range test.files {
				c.Assert(afero.WriteFile(mfs, filepath.Join(baseDir, filename), []byte("let foo='bar';"), 0o777), qt.IsNil)
			}

			conf := testconfig.GetTestConfig(mfs, config.New())
			fs := hugofs.NewFrom(mfs, conf.BaseConfig())

			p, err := paths.New(fs, conf)
			c.Assert(err, qt.IsNil)
			bfs, err := filesystems.NewBase(p, nil)
			c.Assert(err, qt.IsNil)

			got := resolveComponentInAssets(bfs.Assets.Fs, test.impPath)

			gotPath := ""
			expect := test.expect
			if got != nil {
				gotPath = filepath.ToSlash(got.Filename)
				expect = path.Join(baseDir, test.expect)
			}

			c.Assert(gotPath, qt.Equals, expect)
		})
	}
}
