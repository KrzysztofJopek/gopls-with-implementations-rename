// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/gopls/internal/lsp/safetoken"
	"golang.org/x/tools/gopls/internal/span"
	"golang.org/x/tools/internal/diff"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/refactor/satisfy"
)

type renamer struct {
	ctx                context.Context
	fset               *token.FileSet
	refs               []*ReferenceInfo
	objsToUpdate       map[types.Object]bool
	hadConflicts       bool
	errors             string
	from, to           string
	satisfyConstraints map[satisfy.Constraint]bool
	packages           map[*types.Package]Package // may include additional packages that are a dep of pkg.
	msets              typeutil.MethodSetCache
	changeMethods      bool
}

type PrepareItem struct {
	Range protocol.Range
	Text  string
}

// PrepareRename searches for a valid renaming at position pp.
//
// The returned usererr is intended to be displayed to the user to explain why
// the prepare fails. Probably we could eliminate the redundancy in returning
// two errors, but for now this is done defensively.
func PrepareRename(ctx context.Context, snapshot Snapshot, f FileHandle, pp protocol.Position) (_ *PrepareItem, usererr, err error) {
	// Find position of the package name declaration.
	ctx, done := event.Start(ctx, "source.PrepareRename")
	defer done()
	pgf, err := snapshot.ParseGo(ctx, f, ParseFull)
	if err != nil {
		return nil, err, err
	}
	inPackageName, err := isInPackageName(ctx, snapshot, f, pgf, pp)
	if err != nil {
		return nil, err, err
	}

	if inPackageName {
		fileRenameSupported := false
		for _, op := range snapshot.View().Options().SupportedResourceOperations {
			if op == protocol.Rename {
				fileRenameSupported = true
				break
			}
		}

		if !fileRenameSupported {
			err := errors.New("can't rename package: LSP client does not support file renaming")
			return nil, err, err
		}
		fileMeta, err := snapshot.MetadataForFile(ctx, f.URI())
		if err != nil {
			return nil, err, err
		}

		if len(fileMeta) == 0 {
			err := fmt.Errorf("no packages found for file %q", f.URI())
			return nil, err, err
		}

		meta := fileMeta[0]

		if meta.PackageName() == "main" {
			err := errors.New("can't rename package \"main\"")
			return nil, err, err
		}

		if strings.HasSuffix(meta.PackageName(), "_test") {
			err := errors.New("can't rename x_test packages")
			return nil, err, err
		}

		if meta.ModuleInfo() == nil {
			err := fmt.Errorf("can't rename package: missing module information for package %q", meta.PackagePath())
			return nil, err, err
		}

		if meta.ModuleInfo().Path == meta.PackagePath() {
			err := fmt.Errorf("can't rename package: package path %q is the same as module path %q", meta.PackagePath(), meta.ModuleInfo().Path)
			return nil, err, err
		}
		// TODO(rfindley): we should not need the package here.
		pkg, err := snapshot.WorkspacePackageByID(ctx, meta.PackageID())
		if err != nil {
			err = fmt.Errorf("error building package to rename: %v", err)
			return nil, err, err
		}
		result, err := computePrepareRenameResp(snapshot, pkg, pgf.File.Name, pkg.Name())
		if err != nil {
			return nil, nil, err
		}
		return result, nil, nil
	}

	qos, err := qualifiedObjsAtProtocolPos(ctx, snapshot, f.URI(), pp)
	if err != nil {
		return nil, nil, err
	}
	node, obj, pkg := qos[0].node, qos[0].obj, qos[0].sourcePkg
	if err := checkRenamable(obj); err != nil {
		return nil, nil, err
	}
	result, err := computePrepareRenameResp(snapshot, pkg, node, obj.Name())
	if err != nil {
		return nil, nil, err
	}
	return result, nil, nil
}

func computePrepareRenameResp(snapshot Snapshot, pkg Package, node ast.Node, text string) (*PrepareItem, error) {
	mr, err := posToMappedRange(snapshot.FileSet(), pkg, node.Pos(), node.End())
	if err != nil {
		return nil, err
	}
	rng, err := mr.Range()
	if err != nil {
		return nil, err
	}
	if _, isImport := node.(*ast.ImportSpec); isImport {
		// We're not really renaming the import path.
		rng.End = rng.Start
	}
	return &PrepareItem{
		Range: rng,
		Text:  text,
	}, nil
}

// checkRenamable verifies if an obj may be renamed.
func checkRenamable(obj types.Object) error {
	if v, ok := obj.(*types.Var); ok && v.Embedded() {
		return errors.New("can't rename embedded fields: rename the type directly or name the field")
	}
	if obj.Name() == "_" {
		return errors.New("can't rename \"_\"")
	}
	return nil
}

type OptionalEdits struct {
	Edits       map[span.URI][]protocol.TextEdit
	Annotations map[protocol.ChangeAnnotationIdentifier]protocol.ChangeAnnotation
}

// Rename returns a map of TextEdits for each file modified when renaming a
// given identifier within a package and a boolean value of true for renaming
// package and false otherwise.
func Rename(ctx context.Context, s Snapshot, f FileHandle, pp protocol.Position, newName string) (map[span.URI][]protocol.TextEdit, *OptionalEdits, bool, error) {
	ctx, done := event.Start(ctx, "source.Rename")
	defer done()

	pgf, err := s.ParseGo(ctx, f, ParseFull)
	if err != nil {
		return nil, nil, false, err
	}
	inPackageName, err := isInPackageName(ctx, s, f, pgf, pp)
	if err != nil {
		return nil, nil, false, err
	}

	if inPackageName {
		if !isValidIdentifier(newName) {
			return nil, nil, true, fmt.Errorf("%q is not a valid identifier", newName)
		}

		fileMeta, err := s.MetadataForFile(ctx, f.URI())
		if err != nil {
			return nil, nil, true, err
		}

		if len(fileMeta) == 0 {
			return nil, nil, true, fmt.Errorf("no packages found for file %q", f.URI())
		}

		// We need metadata for the relevant package and module paths. These should
		// be the same for all packages containing the file.
		//
		// TODO(rfindley): we mix package path and import path here haphazardly.
		// Fix this.
		meta := fileMeta[0]
		oldPath := meta.PackagePath()
		var modulePath string
		if mi := meta.ModuleInfo(); mi == nil {
			return nil, nil, true, fmt.Errorf("cannot rename package: missing module information for package %q", meta.PackagePath())
		} else {
			modulePath = mi.Path
		}

		if strings.HasSuffix(newName, "_test") {
			return nil, nil, true, fmt.Errorf("cannot rename to _test package")
		}

		metadata, err := s.AllValidMetadata(ctx)
		if err != nil {
			return nil, nil, true, err
		}

		renamingEdits, err := renamePackage(ctx, s, modulePath, oldPath, newName, metadata)
		if err != nil {
			return nil, nil, true, err
		}

		return renamingEdits, nil, true, nil
	}

	qos, err := qualifiedObjsAtProtocolPos(ctx, s, f.URI(), pp)
	if err != nil {
		return nil, nil, false, err
	}
	result, err := renameObj(ctx, s, newName, qos, false)
	if err != nil {
		return nil, nil, false, err
	}
	// If renaming interface signature, then use optional annotation for interface implementations edits
	if isInterfaceSignature(qos[0].obj) {
		annotatedEdits := make(map[span.URI][]protocol.TextEdit)
		annotations := make(map[protocol.ChangeAnnotationIdentifier]protocol.ChangeAnnotation)
		impls, err := implementations(ctx, s, f, pp)
		if err != nil {
			return nil, nil, false, err
		}
		for implID, impl := range impls {
			subResult, err := renameObj(ctx, s, newName, []qualifiedObject{impl}, true)
			if err != nil {
				return nil, nil, false, err
			}
			for uri, res := range subResult {
				for _, te := range res {
					te.AnnotationID = fmt.Sprint(implID)
					annotatedEdits[uri] = append(annotatedEdits[uri], te)
				}
			}
			name := impl.obj.Name()
			if sig, ok := impl.obj.Type().(*types.Signature); ok {
				name = fmt.Sprintf("%s.%s", sig.Recv().Type().String(), name)
			}
			annotations[fmt.Sprint(implID)] = protocol.ChangeAnnotation{
				Label:             fmt.Sprintf("Rename implementation #%d", implID+1),
				NeedsConfirmation: true,
				Description:       name,
			}
		}
		return result, &OptionalEdits{Annotations: annotations, Edits: annotatedEdits}, false, nil
	}

	return result, nil, false, nil
}

// renamePackage computes all workspace edits required to rename the package
// described by the given metadata, to newName, by renaming its package
// directory.
//
// It updates package clauses and import paths for the renamed package as well
// as any other packages affected by the directory renaming among packages
// described by allMetadata.
func renamePackage(ctx context.Context, s Snapshot, modulePath, oldPath, newName string, allMetadata []Metadata) (map[span.URI][]protocol.TextEdit, error) {
	if modulePath == oldPath {
		return nil, fmt.Errorf("cannot rename package: module path %q is the same as the package path, so renaming the package directory would have no effect", modulePath)
	}

	newPathPrefix := path.Join(path.Dir(oldPath), newName)

	edits := make(map[span.URI][]protocol.TextEdit)
	seen := make(seenPackageRename) // track per-file import renaming we've already processed

	// Rename imports to the renamed package from other packages.
	for _, m := range allMetadata {
		// Special case: x_test packages for the renamed package will not have the
		// package path as as a dir prefix, but still need their package clauses
		// renamed.
		if m.PackagePath() == oldPath+"_test" {
			newTestName := newName + "_test"

			if err := renamePackageClause(ctx, m, s, newTestName, seen, edits); err != nil {
				return nil, err
			}
			continue
		}

		// Subtle: check this condition before checking for valid module info
		// below, because we should not fail this operation if unrelated packages
		// lack module info.
		if !strings.HasPrefix(m.PackagePath()+"/", oldPath+"/") {
			continue // not affected by the package renaming
		}

		if m.ModuleInfo() == nil {
			return nil, fmt.Errorf("cannot rename package: missing module information for package %q", m.PackagePath())
		}

		if modulePath != m.ModuleInfo().Path {
			continue // don't edit imports if nested package and renaming package have different module paths
		}

		// Renaming a package consists of changing its import path and package name.
		suffix := strings.TrimPrefix(m.PackagePath(), oldPath)
		newPath := newPathPrefix + suffix

		pkgName := m.PackageName()
		if m.PackagePath() == oldPath {
			pkgName = newName

			if err := renamePackageClause(ctx, m, s, newName, seen, edits); err != nil {
				return nil, err
			}
		}

		if err := renameImports(ctx, s, m, newPath, pkgName, seen, edits); err != nil {
			return nil, err
		}
	}

	return edits, nil
}

// seenPackageRename tracks import path renamings that have already been
// processed.
//
// Due to test variants, files may appear multiple times in the reverse
// transitive closure of a renamed package, or in the reverse transitive
// closure of different variants of a renamed package (both are possible).
// However, in all cases the resulting edits will be the same.
type seenPackageRename map[seenPackageKey]bool
type seenPackageKey struct {
	uri        span.URI
	importPath string
}

// add reports whether uri and importPath have been seen, and records them as
// seen if not.
func (s seenPackageRename) add(uri span.URI, importPath string) bool {
	key := seenPackageKey{uri, importPath}
	seen := s[key]
	if !seen {
		s[key] = true
	}
	return seen
}

// renamePackageClause computes edits renaming the package clause of files in
// the package described by the given metadata, to newName.
//
// As files may belong to multiple packages, the seen map tracks files whose
// package clause has already been updated, to prevent duplicate edits.
//
// Edits are written into the edits map.
func renamePackageClause(ctx context.Context, m Metadata, s Snapshot, newName string, seen seenPackageRename, edits map[span.URI][]protocol.TextEdit) error {
	pkg, err := s.WorkspacePackageByID(ctx, m.PackageID())
	if err != nil {
		return err
	}

	// Rename internal references to the package in the renaming package.
	for _, f := range pkg.CompiledGoFiles() {
		if seen.add(f.URI, m.PackagePath()) {
			continue
		}

		if f.File.Name == nil {
			continue
		}
		pkgNameMappedRange := NewMappedRange(f.Tok, f.Mapper, f.File.Name.Pos(), f.File.Name.End())
		rng, err := pkgNameMappedRange.Range()
		if err != nil {
			return err
		}
		edits[f.URI] = append(edits[f.URI], protocol.TextEdit{
			Range:   rng,
			NewText: newName,
		})
	}

	return nil
}

// renameImports computes the set of edits to imports resulting from renaming
// the package described by the given metadata, to a package with import path
// newPath and name newName.
//
// Edits are written into the edits map.
func renameImports(ctx context.Context, s Snapshot, m Metadata, newPath, newName string, seen seenPackageRename, edits map[span.URI][]protocol.TextEdit) error {
	// TODO(rfindley): we should get reverse dependencies as metadata first,
	// rather then building the package immediately. We don't need reverse
	// dependencies if they are intermediate test variants.
	rdeps, err := s.GetReverseDependencies(ctx, m.PackageID())
	if err != nil {
		return err
	}

	for _, dep := range rdeps {
		// Subtle: don't perform renaming in this package if it is not fully
		// parsed. This can occur inside the workspace if dep is an intermediate
		// test variant. ITVs are only ever parsed in export mode, and no file is
		// found only in an ITV. Therefore the renaming will eventually occur in a
		// full package.
		//
		// An alternative algorithm that may be more robust would be to first
		// collect *files* that need to have their imports updated, and then
		// perform the rename using s.PackageForFile(..., NarrowestPackage).
		if dep.ParseMode() != ParseFull {
			continue
		}

		for _, f := range dep.CompiledGoFiles() {
			if seen.add(f.URI, m.PackagePath()) {
				continue
			}

			for _, imp := range f.File.Imports {
				if impPath, _ := strconv.Unquote(imp.Path.Value); impPath != m.PackagePath() {
					continue // not the import we're looking for
				}

				// Create text edit for the import path (string literal).
				impPathMappedRange := NewMappedRange(f.Tok, f.Mapper, imp.Path.Pos(), imp.Path.End())
				rng, err := impPathMappedRange.Range()
				if err != nil {
					return err
				}
				newText := strconv.Quote(newPath)
				edits[f.URI] = append(edits[f.URI], protocol.TextEdit{
					Range:   rng,
					NewText: newText,
				})

				// If the package name of an import has not changed or if its import
				// path already has a local package name, then we don't need to update
				// the local package name.
				if newName == m.PackageName() || imp.Name != nil {
					continue
				}

				// Rename the types.PkgName locally within this file.
				pkgname := dep.GetTypesInfo().Implicits[imp].(*types.PkgName)
				qos := []qualifiedObject{{obj: pkgname, pkg: dep}}

				pkgScope := dep.GetTypes().Scope()
				fileScope := dep.GetTypesInfo().Scopes[f.File]

				var changes map[span.URI][]protocol.TextEdit
				localName := newName
				try := 0

				// Keep trying with fresh names until one succeeds.
				for fileScope.Lookup(localName) != nil || pkgScope.Lookup(localName) != nil {
					try++
					localName = fmt.Sprintf("%s%d", newName, try)
				}
				changes, err = renameObj(ctx, s, localName, qos, false)
				if err != nil {
					return err
				}

				// If the chosen local package name matches the package's new name, delete the
				// change that would have inserted an explicit local name, which is always
				// the lexically first change.
				if localName == newName {
					v := changes[f.URI]
					sort.Slice(v, func(i, j int) bool {
						return protocol.CompareRange(v[i].Range, v[j].Range) < 0
					})
					changes[f.URI] = v[1:]
				}
				for uri, changeEdits := range changes {
					edits[uri] = append(edits[uri], changeEdits...)
				}
			}
		}
	}

	return nil
}

// renameObj returns a map of TextEdits for renaming an identifier within a file
// and boolean value of true if there is no renaming conflicts and false otherwise.
func renameObj(ctx context.Context, s Snapshot, newName string, qos []qualifiedObject, renameImpls bool) (map[span.URI][]protocol.TextEdit, error) {
	obj := qos[0].obj

	if err := checkRenamable(obj); err != nil {
		return nil, err
	}
	if obj.Name() == newName {
		return nil, fmt.Errorf("old and new names are the same: %s", newName)
	}
	if !isValidIdentifier(newName) {
		return nil, fmt.Errorf("invalid identifier to rename: %q", newName)
	}

	refs, err := references(ctx, s, qos, true, false, true)
	if err != nil {
		return nil, err
	}
	r := renamer{
		ctx:          ctx,
		fset:         s.FileSet(),
		refs:         refs,
		objsToUpdate: make(map[types.Object]bool),
		from:         obj.Name(),
		to:           newName,
		packages:     make(map[*types.Package]Package),
	}

	// A renaming initiated at an interface method indicates the
	// intention to rename abstract and concrete methods as needed
	// to preserve assignability.
	if renameImpls {
		r.changeMethods = true
	} else {
		for _, ref := range refs {
			if obj, ok := ref.obj.(*types.Func); ok {
				recv := obj.Type().(*types.Signature).Recv()
				if recv != nil && IsInterface(recv.Type().Underlying()) {
					r.changeMethods = true
					break
				}
			}
		}
	}
	for _, from := range refs {
		r.packages[from.pkg.GetTypes()] = from.pkg
	}

	// Check that the renaming of the identifier is ok.
	for _, ref := range refs {
		r.check(ref.obj)
		if r.hadConflicts { // one error is enough.
			break
		}
	}
	if r.hadConflicts {
		return nil, fmt.Errorf(r.errors)
	}

	changes, err := r.update()
	if err != nil {
		return nil, err
	}

	result := make(map[span.URI][]protocol.TextEdit)
	for uri, edits := range changes {
		// These edits should really be associated with FileHandles for maximal correctness.
		// For now, this is good enough.
		fh, err := s.GetFile(ctx, uri)
		if err != nil {
			return nil, err
		}
		data, err := fh.Read()
		if err != nil {
			return nil, err
		}
		m := protocol.NewColumnMapper(uri, data)
		protocolEdits, err := ToProtocolEdits(m, edits)
		if err != nil {
			return nil, err
		}
		result[uri] = protocolEdits
	}
	return result, nil
}

func isInterfaceSignature(obj types.Object) bool {
	if obj, ok := obj.(*types.Func); ok {
		return types.IsInterface(obj.Type().(*types.Signature).Recv().Type().Underlying())
	}
	return false
}

// Rename all references to the identifier.
func (r *renamer) update() (map[span.URI][]diff.Edit, error) {
	result := make(map[span.URI][]diff.Edit)
	seen := make(map[span.Span]bool)

	docRegexp, err := regexp.Compile(`\b` + r.from + `\b`)
	if err != nil {
		return nil, err
	}
	for _, ref := range r.refs {
		refSpan, err := ref.Span()
		if err != nil {
			return nil, err
		}
		if seen[refSpan] {
			continue
		}
		seen[refSpan] = true

		// Renaming a types.PkgName may result in the addition or removal of an identifier,
		// so we deal with this separately.
		if pkgName, ok := ref.obj.(*types.PkgName); ok && ref.isDeclaration {
			edit, err := r.updatePkgName(pkgName)
			if err != nil {
				return nil, err
			}
			result[refSpan.URI()] = append(result[refSpan.URI()], *edit)
			continue
		}

		// Replace the identifier with r.to.
		edit := diff.Edit{
			Start: refSpan.Start().Offset(),
			End:   refSpan.End().Offset(),
			New:   r.to,
		}

		result[refSpan.URI()] = append(result[refSpan.URI()], edit)

		if !ref.isDeclaration || ref.ident == nil { // uses do not have doc comments to update.
			continue
		}

		doc := r.docComment(ref.pkg, ref.ident)
		if doc == nil {
			continue
		}

		// Perform the rename in doc comments declared in the original package.
		// go/parser strips out \r\n returns from the comment text, so go
		// line-by-line through the comment text to get the correct positions.
		for _, comment := range doc.List {
			if isDirective(comment.Text) {
				continue
			}
			// TODO(adonovan): why are we looping over lines?
			// Just run the loop body once over the entire multiline comment.
			lines := strings.Split(comment.Text, "\n")
			tokFile := r.fset.File(comment.Pos())
			commentLine := tokFile.Line(comment.Pos())
			uri := span.URIFromPath(tokFile.Name())
			for i, line := range lines {
				lineStart := comment.Pos()
				if i > 0 {
					lineStart = tokFile.LineStart(commentLine + i)
				}
				for _, locs := range docRegexp.FindAllIndex([]byte(line), -1) {
					// The File.Offset static check complains
					// even though these uses are manifestly safe.
					start, _ := safetoken.Offset(tokFile, lineStart+token.Pos(locs[0]))
					end, _ := safetoken.Offset(tokFile, lineStart+token.Pos(locs[1]))
					result[uri] = append(result[uri], diff.Edit{
						Start: start,
						End:   end,
						New:   r.to,
					})
				}
			}
		}
	}

	return result, nil
}

// docComment returns the doc for an identifier.
func (r *renamer) docComment(pkg Package, id *ast.Ident) *ast.CommentGroup {
	_, tokFile, nodes, _ := pathEnclosingInterval(r.fset, pkg, id.Pos(), id.End())
	for _, node := range nodes {
		switch decl := node.(type) {
		case *ast.FuncDecl:
			return decl.Doc
		case *ast.Field:
			return decl.Doc
		case *ast.GenDecl:
			return decl.Doc
		// For {Type,Value}Spec, if the doc on the spec is absent,
		// search for the enclosing GenDecl
		case *ast.TypeSpec:
			if decl.Doc != nil {
				return decl.Doc
			}
		case *ast.ValueSpec:
			if decl.Doc != nil {
				return decl.Doc
			}
		case *ast.Ident:
		case *ast.AssignStmt:
			// *ast.AssignStmt doesn't have an associated comment group.
			// So, we try to find a comment just before the identifier.

			// Try to find a comment group only for short variable declarations (:=).
			if decl.Tok != token.DEFINE {
				return nil
			}

			identLine := tokFile.Line(id.Pos())
			for _, comment := range nodes[len(nodes)-1].(*ast.File).Comments {
				if comment.Pos() > id.Pos() {
					// Comment is after the identifier.
					continue
				}

				lastCommentLine := tokFile.Line(comment.End())
				if lastCommentLine+1 == identLine {
					return comment
				}
			}
		default:
			return nil
		}
	}
	return nil
}

// updatePkgName returns the updates to rename a pkgName in the import spec by
// only modifying the package name portion of the import declaration.
func (r *renamer) updatePkgName(pkgName *types.PkgName) (*diff.Edit, error) {
	// Modify ImportSpec syntax to add or remove the Name as needed.
	pkg := r.packages[pkgName.Pkg()]
	_, tokFile, path, _ := pathEnclosingInterval(r.fset, pkg, pkgName.Pos(), pkgName.Pos())
	if len(path) < 2 {
		return nil, fmt.Errorf("no path enclosing interval for %s", pkgName.Name())
	}
	spec, ok := path[1].(*ast.ImportSpec)
	if !ok {
		return nil, fmt.Errorf("failed to update PkgName for %s", pkgName.Name())
	}

	newText := ""
	if pkgName.Imported().Name() != r.to {
		newText = r.to + " "
	}

	// Replace the portion (possibly empty) of the spec before the path:
	//     local "path"      or      "path"
	//   ->      <-                -><-
	rng := span.NewRange(tokFile, spec.Pos(), spec.Path.Pos())
	spn, err := rng.Span()
	if err != nil {
		return nil, err
	}

	return &diff.Edit{
		Start: spn.Start().Offset(),
		End:   spn.End().Offset(),
		New:   newText,
	}, nil
}
