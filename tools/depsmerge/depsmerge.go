// CookieJar - A contestant's algorithm toolbox
// Copyright 2013 Peter Szilagyi. All rights reserved.
//
// CookieJar is dual licensed: you can redistribute it and/or modify it under
// the terms of the GNU General Public License as published by the Free Software
// Foundation, either version 3 of the License, or (at your option) any later
// version.
//
// The toolbox is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE.  See the GNU General Public License for
// more details.
//
// Alternatively, the CookieJar toolbox may be used in accordance with the terms
// and conditions contained in a signed written agreement between you and the
// author(s).
//
// Author: peterke@gmail.com (Peter Szilagyi)

// Command depsmerge implements a command to retrieve and merge all dependencies
// of a package into a single file.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"code.google.com/p/go.tools/imports"
	"gopkg.in/inconshreveable/log15.v2"
)

// Package description from the go list command
type Package struct {
	Name     string
	Dir      string
	Standard bool
	Deps     []string
	GoFiles  []string
}

// Loads the details of the Go package.
func details(name string) (*Package, error) {
	// Create the command to retrieve the package infos
	cmd := exec.Command("go", "list", "-e", "-json", name)

	// Retrieve the output, redirect the errors
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr

	// Start executing and parse the results
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer cmd.Process.Kill()

	info := new(Package)
	if err := json.NewDecoder(out).Decode(&info); err != nil {
		return nil, err
	}
	// Clean up and return
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return info, nil
}

// Collects all the imported packages of a file.
func dependencies(path string) (map[string][]string, error) {
	// Retrieve the dependencies of the source file
	info, err := details(path)
	if err != nil {
		return nil, err
	}
	// Iterate over every dependency and gather the sources
	sources := make(map[string][]string)
	for _, dep := range info.Deps {
		// Retrieve the dependency details
		pkgInfo, err := details(dep)
		if err != nil {
			return nil, err
		}
		// Gather external library sources
		if !pkgInfo.Standard {
			for _, src := range pkgInfo.GoFiles {
				sources[pkgInfo.Name] = append(sources[pkgInfo.Name], filepath.Join(pkgInfo.Dir, src))
			}
		}
	}
	return sources, nil
}

// Iterates over a package contents and collects all declarations to rename.
func declarations(paths []string) ([]*ast.Object, error) {
	results := []*ast.Object{}
	for _, path := range paths {
		// Parse the specified source file
		fileSet := token.NewFileSet()
		tree, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		// Collect all top level declarations
		for _, decl := range tree.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil && decl.Name.Name != "init" {
					results = append(results, ast.NewObj(ast.Fun, decl.Name.String()))
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							results = append(results, ast.NewObj(ast.Var, name.String()))
						}
					case *ast.TypeSpec:
						results = append(results, ast.NewObj(ast.Typ, spec.Name.String()))
					default:
						log15.Warn("Unknown specification", "spec", spec)
					}
				}
			default:
				log15.Warn("Unknown declaration", "decl", decl)
			}
		}
	}
	return results, nil
}

// Parses a source file and scopes all global declarations.
func rewrite(src string, pkg string, decls []*ast.Object) ([]byte, error) {
	fileSet := token.NewFileSet()
	tree, err := parser.ParseFile(fileSet, src, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	// Scope all top level declarations if not main file
	if pkg != "" {
		for _, decl := range decls {
			rename(tree, decl.Name, pkg+"ᴥ"+decl.Name, decl.Kind)
		}
	}
	// Generate the new source contents
	out := bytes.NewBuffer(nil)
	for _, decl := range tree.Decls {
		if gen, ok := decl.(*ast.GenDecl); !ok || gen.Tok != token.IMPORT {
			if err := printer.Fprint(out, fileSet, decl); err != nil {
				return nil, err
			}
		}
		fmt.Fprintf(out, "\n\n")
	}
	blob := out.Bytes()

	// Scope all externally imported dependencies
	var fail error
	ast.Inspect(tree, func(node ast.Node) bool {
		if imp, ok := node.(*ast.GenDecl); ok && imp.Tok == token.IMPORT {
			for _, spec := range imp.Specs {
				if spec, ok := spec.(*ast.ImportSpec); ok {
					// Figure out the correct name of the import
					path := strings.Trim(spec.Path.Value, "\"")
					if info, err := details(path); err != nil {
						fail = err
						return false
					} else if !info.Standard {
						// Add scope to external import
						scoper := regexp.MustCompile("([^\\.])" + info.Name + "\\.(.+)")
						blob = scoper.ReplaceAll(blob, []byte("${1}"+info.Name+"ᴥ${2}"))
					}
				}
			}
		}
		return true
	})
	if fail != nil {
		return nil, fail
	}
	return blob, nil
}

// Renames a top level declaration to something else.
func rename(tree *ast.File, old, new string, kind ast.ObjKind) {
	// Rename top-level declarations
	for _, decl := range tree.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			// If a top level function matches, rename
			if decl.Recv == nil && decl.Name.Name == old {
				decl.Name.Name = new
				if decl.Name.Obj != nil {
					decl.Name.Obj.Name = new
				}
			}
		case *ast.GenDecl:
			// Iterate over all the generic declaration
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					// If a top level variable matches, rename
					for _, name := range spec.Names {
						if name.Name == old {
							name.Name = new
							if name.Obj != nil {
								name.Obj.Name = new
							}
						}
					}
				case *ast.TypeSpec:
					if spec.Name.Name == old {
						spec.Name.Name = new
						if spec.Name.Obj != nil {
							spec.Name.Obj.Name = new
						}
					}
				}
			}
		}
	}
	// Walk the AST and rename all internal occurrences
	stack := []ast.Node{}
	ast.Inspect(tree, func(node ast.Node) bool {
		// Keep a traversal stack if need to reference parent
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, node)

		// Look for identifiers to rename
		id, ok := node.(*ast.Ident)
		if ok && id.Obj == nil && id.Name == old {
			// Don't rename selected identifiers, member functions, struct keys
			switch stack[len(stack)-2].(type) {
			case *ast.SelectorExpr, *ast.FuncDecl:
				return true
			case *ast.KeyValueExpr:
				// If the rename is a variable, allow it
				if kind != ast.Var && kind != ast.Con {
					return true
				}
			}
			id.Name = new
		}
		if ok && id.Obj != nil && id.Name == old && id.Obj.Name == new {
			// Don't rename struct keys
			switch stack[len(stack)-2].(type) {
			case *ast.KeyValueExpr:
				// If the rename is a variable, allow it
				if kind != ast.Var && kind != ast.Con {
					return true
				}
			}
			id.Name = new
		}
		return true
	})
}

// Flattens a main file with rewritten dependencies.
func flatten(pkg string, main []byte, deps [][]byte) ([]byte, error) {
	buffer := new(bytes.Buffer)

	// Dump all code pieces into a buffer
	fmt.Fprintf(buffer, "package %s\n\n", pkg)
	fmt.Fprintf(buffer, "%s\n\n", main)
	for _, dep := range deps {
		fmt.Fprintf(buffer, "%s\n\n", dep)
	}
	// Format the blob to Go standards and add imports
	blob, err := imports.Process("", buffer.Bytes(), nil)
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// Some configuration flags to override the defaults
var outPkg = flag.String("pkg", "main", "Package name to generate for the merged file")
var outName = flag.String("out", "", "Source file to generate (empty = stdout)")

func main() {
	flag.Parse()

	deps, err := dependencies(flag.Args()[0])
	if err != nil {
		log.Fatalf("Failed to parse dependency chain: %v.", err)
	}
	// Rewrite all the dependencies
	pieces := make([][]byte, 0, len(deps))
	for pkg, sources := range deps {
		// Collect all the declarations in need of rewriting
		decls, err := declarations(sources)
		if err != nil {
			log.Fatalf("Failed to collect top level declarations %s: %v", pkg, err)
		}
		// Rewrite each of them in each source file
		for _, src := range sources {
			blob, err := rewrite(src, pkg, decls)
			if err != nil {
				log.Fatalf("Failed to rewrite dependency %s: %v.", src, err)
			}
			pieces = append(pieces, blob)
		}
	}
	// Rewrite the main file itself, append all dependencies
	main, err := rewrite(flag.Args()[0], "", nil)
	if err != nil {
		log.Fatalf("Failed to rewrite main file %s: %v.", flag.Args()[0], err)
	}
	blob, err := flatten(*outPkg, main, pieces)
	if err != nil {
		log.Fatalf("Failed to flatten and format sources: %v.", err)
	}
	if *outName == "" {
		fmt.Println(string(blob))
	} else {
		ioutil.WriteFile(*outName, blob, 0700)
	}
}
