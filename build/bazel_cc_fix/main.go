// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// bazel_cc_fix rewrites include directives in C and C++ code. It rewrites all includes in the target workspace to be
// workspace-relative and additionally supports rewriting includes via a prototxt-based spec file to for example
// fix up includes for external libraries.
// The rewritten code can then be used in Bazel intra- and inter-workspace without dealing with any copts or include-
// related attributes.
// To know where an include would resolve to it expects a compilation database (see
// https://clang.llvm.org/docs/JSONCompilationDatabase.html) as an input. It looks at all files in that database and
// their transitive dependencies and rewrites all of them according to the include paths specified in the compilation
// command from the database.
// The compilation database itself is either generated by the original build system or by using intercept-build, which
// intercepts calls to the compiler and records them into a compilation database.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/mattn/go-shellwords"

	"source.monogon.dev/build/bazel_cc_fix/ccfixspec"
)

// compilationDBEntry is a single entry from the compilation database which represents a single compiler invocation on
// a C/C++ source file. It contains the compiler working directory, arguments and input file path.
type compilationDBEntry struct {
	Directory string   `json:"directory"`
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
	File      string   `json:"file"`
	Output    string   `json:"output"`
}

// compilationDB is a collection of compilationDBEntries usually stored in a big JSON-serialized document.
// https://clang.llvm.org/docs/JSONCompilationDatabase.html
type compilationDB []compilationDBEntry

// rewrites represents a list of include rewrites with the key being the original include statement
// (like "#include <xyz.h>", with whitespace trimmed on both sides) and the value being another
type rewrites map[string]string

// replacer returns a strings.Replacer which efficiently performs all replacements in a single pass
func (r rewrites) replacer() *strings.Replacer {
	var replacerArgs []string
	for from, to := range r {
		replacerArgs = append(replacerArgs, from, to)
	}
	return strings.NewReplacer(replacerArgs...)
}

// addWorkspace adds a rewrite from a given directive to a workspace-relative path.
func (r rewrites) addWorkspace(oldDirective, workspaceRelativePath string) {
	normalizedDirective := strings.TrimSpace(oldDirective)
	replacementDirective := fmt.Sprintf("#include \"%s\"", workspaceRelativePath)
	oldRewrite, ok := r[normalizedDirective]
	if !ok {
		r[normalizedDirective] = replacementDirective
	} else if oldRewrite != replacementDirective {
		log.Printf("WARNING: inconsistent rewrite detected: %s => %s | %s", normalizedDirective, oldRewrite, replacementDirective)
	}
}

// Type rewriteMetadata is a map of a file path to rewrite metadata for that file
type rewriteMetadata map[string]rewriteMetadataFile

type rewriteMetadataFile struct {
	rewrites rewrites
	source   string
}

var (
	compilationDBPath = flag.String("compilation_db", "", "Path the the compilation_database.json file for the project")
	workspacePath     = flag.String("workspace", "", "Path to the workspace root")
	specPath          = flag.String("spec", "", "Path to the spec (ccfixspec.CCFixSpec)")
)

var (
	reGlobalInclude = regexp.MustCompile("^-I(.*)")
	reSystemInclude = regexp.MustCompile("^-isystem(.*)")
	reQuoteInclude  = regexp.MustCompile("^-iquote(.*)")
)

var (
	reIncludeDirective = regexp.MustCompile(`(?m:^\s*#\s*include\s*([<"])(.*)([>"]))`)
)

// applyReplaceDirectives applies all directives of the given replaceType in directives to originalPath and returns the
// resulting string. If returnUnmodified is unset, it returns an empty string when no replacements were performed,
// otherwise it returns the unmodified originalPath.
// The first rewrite wins, it does not do any recursive processing.
func applyReplaceDirectives(directives []*ccfixspec.Replace, replaceType ccfixspec.Replace_Type, originalPath string, returnUnmodified bool) string {
	for _, d := range directives {
		if d.Type != replaceType {
			continue
		}
		if d.From == originalPath {
			return d.To
		} else if strings.HasSuffix(d.From, "/") && strings.HasPrefix(originalPath, d.From) {
			return d.To + strings.TrimPrefix(originalPath, d.From)
		}
	}
	if returnUnmodified {
		return originalPath
	}
	return ""
}

// findFileInWorkspace takes a path from a C include directive and uses the given search path to find its absolute
// path. If that absolute path is outside the workspace, it returns an empty string, otherwise it returns the path
// of the file relative to the workspace. It pretends that all files in isGeneratedFile exist on the filesystem.
func findFileInWorkspace(searchPath []string, inclFile string, isGeneratedFile map[string]bool) string {
	var inclPath string
	for _, path := range searchPath {
		inclPathTry := filepath.Join(path, inclFile)
		if isGeneratedFile[inclPathTry] {
			inclPath = inclPathTry
			break
		}
		if _, err := os.Stat(inclPathTry); err == nil {
			inclPath = inclPathTry
			break
		}
	}
	if inclPath == "" {
		// We haven't found the included file. This can happen for system includes (<stdio.h>) or includes from
		// other operating systems.
		return ""
	}

	// Ignore all include directives that don't resolve into our workspace after processing
	if !filepath.HasPrefix(inclPath, *workspacePath) {
		return ""
	}

	workspaceRelativeFilePath, err := filepath.Rel(*workspacePath, inclPath)
	if err != nil {
		panic(err)
	}
	return workspaceRelativeFilePath
}

// fixIncludesAndGetRefs opens a file, looks at all its includes, records rewriting data into rewriteMetadata and
// returns all files included by the file for further analysis.
func (m rewriteMetadata) fixIncludesAndGetRefs(filePath string, quoteIncludes, systemIncludes []string, spec *ccfixspec.CCFixSpec, isGeneratedFile map[string]bool) []string {
	meta, ok := m[filePath]
	if !ok {
		cSourceRaw, err := ioutil.ReadFile(filePath)
		if err != nil {
			log.Printf("failed to open source file: %v", err)
			return nil
		}
		cSource := string(cSourceRaw)
		m[filePath] = rewriteMetadataFile{
			rewrites: make(rewrites),
			source:   cSource,
		}
		meta = m[filePath]
	}
	var includeFiles []string
	// Find all include directives
	out := reIncludeDirective.FindAllStringSubmatch(meta.source, -1)
	for _, incl := range out {
		inclDirective := incl[0]
		inclType := incl[1]
		inclFile := incl[2]
		var workspaceRelativeFilePath string
		var searchPath []string
		if inclType == "\"" {
			searchPath = quoteIncludes
		} else if inclType == "<" {
			searchPath = systemIncludes
			workspaceRelativeFilePath = applyReplaceDirectives(spec.Replace, ccfixspec.Replace_SYSTEM, inclFile, false)
		}
		if workspaceRelativeFilePath == "" {
			workspaceRelativeFilePath = findFileInWorkspace(searchPath, inclFile, isGeneratedFile)
		}
		workspaceRelativeFilePath = applyReplaceDirectives(spec.Replace, ccfixspec.Replace_WORKSPACE, workspaceRelativeFilePath, true)

		// Mark generated files as generated
		foundGenerated := isGeneratedFile[filepath.Join(*workspacePath, workspaceRelativeFilePath)]

		if !foundGenerated {
			includeFiles = append(includeFiles, filepath.Join(*workspacePath, workspaceRelativeFilePath))
		}

		// Pretend that a generated file exists at the given path when stripping the BuildDir prefix. This is
		// generally true for all out-of-tree build systems and saves the user from needing to manually specify
		// lots of GeneratedFiles.
		if spec.BuildDir != "" && filepath.HasPrefix(workspaceRelativeFilePath, spec.BuildDir+"/") {
			workspaceRelativeFilePath = filepath.Clean(strings.TrimPrefix(workspaceRelativeFilePath, spec.BuildDir+"/"))
			foundGenerated = true
		}

		// Shorten include paths when both files are in the same directory except when a generated file is involved
		// as these end up in physically different locations and need to be referenced using a full workspace-
		// relative path
		if !foundGenerated && filepath.Dir(filePath) == filepath.Dir(filepath.Join(*workspacePath, workspaceRelativeFilePath)) {
			workspaceRelativeFilePath = filepath.Base(workspaceRelativeFilePath)
		}
		// Don't perform rewrites when both include directives are semantically equivalent
		if workspaceRelativeFilePath == inclFile && inclType == "\"" {
			continue
		}
		meta.rewrites.addWorkspace(inclDirective, workspaceRelativeFilePath)
	}
	return includeFiles
}

// getIncludeDirs takes a compilation database entry and returns the search paths for both system and quote includes
func getIncludeDirs(entry compilationDBEntry) (quoteIncludes []string, systemIncludes []string, err error) {
	// Normalize arguments
	if len(entry.Arguments) == 0 {
		commandArgs, err := shellwords.Parse(entry.Command)
		if err != nil {
			return []string{}, []string{}, fmt.Errorf("failed to parse command: %w", err)
		}
		entry.Arguments = commandArgs
	}

	// Parse out and generate include search paths
	var preSystemIncludes []string
	var systemIncludesRaw []string
	var quoteIncludesRaw []string
	filePath := entry.File
	if !filepath.IsAbs(entry.File) {
		filePath = filepath.Join(entry.Directory, entry.File)
	}
	quoteIncludesRaw = append(quoteIncludesRaw, filepath.Dir(filePath))
	for i, arg := range entry.Arguments {
		includeMatch := reGlobalInclude.FindStringSubmatch(arg)
		if len(includeMatch) > 0 {
			if len(includeMatch[1]) == 0 {
				preSystemIncludes = append(preSystemIncludes, entry.Arguments[i+1])
			} else {
				preSystemIncludes = append(preSystemIncludes, includeMatch[1])
			}
		}
		includeMatch = reSystemInclude.FindStringSubmatch(arg)
		if len(includeMatch) > 0 {
			if len(includeMatch[1]) == 0 {
				systemIncludesRaw = append(systemIncludesRaw, entry.Arguments[i+1])
			} else {
				systemIncludesRaw = append(systemIncludesRaw, includeMatch[1])
			}
		}
		includeMatch = reQuoteInclude.FindStringSubmatch(arg)
		if len(includeMatch) > 0 {
			if len(includeMatch[1]) == 0 {
				quoteIncludesRaw = append(quoteIncludesRaw, entry.Arguments[i+1])
			} else {
				quoteIncludesRaw = append(quoteIncludesRaw, includeMatch[1])
			}
		}
	}
	systemIncludesRaw = append(preSystemIncludes, systemIncludesRaw...)
	quoteIncludesRaw = append(quoteIncludesRaw, systemIncludesRaw...)

	// Deduplicate and keep the first one
	systemIncludeSeen := make(map[string]bool)
	quoteIncludeSeen := make(map[string]bool)
	for _, systemInclude := range systemIncludesRaw {
		if !filepath.IsAbs(systemInclude) {
			systemInclude = filepath.Join(entry.Directory, systemInclude)
		}
		if !systemIncludeSeen[systemInclude] {
			systemIncludeSeen[systemInclude] = true
			systemIncludes = append(systemIncludes, systemInclude)
		}
	}
	for _, quoteInclude := range quoteIncludesRaw {
		if !filepath.IsAbs(quoteInclude) {
			quoteInclude = filepath.Join(entry.Directory, quoteInclude)
		}
		if !quoteIncludeSeen[quoteInclude] {
			quoteIncludeSeen[quoteInclude] = true
			quoteIncludes = append(quoteIncludes, quoteInclude)
		}
	}
	return
}

func main() {
	flag.Parse()
	compilationDBFile, err := os.Open(*compilationDBPath)
	if err != nil {
		log.Fatalf("failed to open compilation db: %v", err)
	}
	var compilationDB compilationDB
	if err := json.NewDecoder(compilationDBFile).Decode(&compilationDB); err != nil {
		log.Fatalf("failed to read compilation db: %v", err)
	}
	specRaw, err := ioutil.ReadFile(*specPath)
	var spec ccfixspec.CCFixSpec
	if err := proto.UnmarshalText(string(specRaw), &spec); err != nil {
		log.Fatalf("failed to load spec: %v", err)
	}

	isGeneratedFile := make(map[string]bool)
	for _, entry := range spec.GeneratedFile {
		isGeneratedFile[filepath.Join(*workspacePath, entry.Path)] = true
	}

	rewriteMetadata := make(rewriteMetadata)

	// Iterate over all source files in the compilation database and analyze them one-by-one
	for _, entry := range compilationDB {
		quoteIncludes, systemIncludes, err := getIncludeDirs(entry)
		if err != nil {
			log.Println(err)
			continue
		}
		filePath := entry.File
		if !filepath.IsAbs(entry.File) {
			filePath = filepath.Join(entry.Directory, entry.File)
		}
		includedFiles := rewriteMetadata.fixIncludesAndGetRefs(filePath, quoteIncludes, systemIncludes, &spec, isGeneratedFile)

		// seen stores the path of already-visited files, similar to #pragma once
		seen := make(map[string]bool)
		// rec recursively resolves includes and records rewrites
		var rec func([]string)
		rec = func(files []string) {
			for _, f := range files {
				if seen[f] {
					continue
				}
				seen[f] = true
				icf2 := rewriteMetadata.fixIncludesAndGetRefs(f, quoteIncludes, systemIncludes, &spec, isGeneratedFile)
				rec(icf2)
			}
		}
		rec(includedFiles)
	}

	// Perform all recorded rewrites on the actual files
	for file, rew := range rewriteMetadata {
		outFile, err := os.Create(file)
		if err != nil {
			log.Fatalf("failed to open file for writing output: %v", err)
		}
		defer outFile.Close()
		if _, err := rew.rewrites.replacer().WriteString(outFile, rew.source); err != nil {
			log.Fatalf("failed to write file %v: %v", file, err)
		}
	}
}
