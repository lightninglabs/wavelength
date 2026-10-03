package actorblock

import (
	"go/ast"
	"go/types"
	"strings"
)

// entries finds the actor turn entry points of the package.
func (st *pkgState) entries(decls []funcDecl) []entry {
	var out []entry

	declByObj := make(map[*types.Func]funcDecl, len(decls))
	for _, d := range decls {
		declByObj[d.obj] = d
		if st.isReceive(d.decl) || st.isExtraEntry(d.decl) {
			out = append(out, entry{
				name: recvName(d.decl) + "." + d.decl.Name.Name,
				body: d.decl.Body,
				key:  d.key,
			})
		}
	}

	// Find function behavior constructors, both inside declarations and
	// in package-level initializers. The key of an entry inside an
	// initializer is synthetic, so it cannot be baselined.
	scan := func(root ast.Node, key string) {
		ast.Inspect(root, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			e, ok := st.behaviorEntry(call, key, declByObj)
			if ok {
				out = append(out, e)
			}

			return true
		})
	}
	for _, f := range st.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				scan(d, st.pass.Pkg.Path()+".init")
				continue
			}
			if fd.Body == nil {
				continue
			}

			obj, _ := st.pass.TypesInfo.Defs[fd.Name].(*types.Func)
			if obj != nil {
				scan(fd.Body, funcKey(obj))
			}
		}
	}

	return out
}

// behaviorEntry returns the entry point that call registers if it is a call
// to a function behavior constructor. key is the baseline key of the function
// that contains the call, used for a function literal argument.
func (st *pkgState) behaviorEntry(call *ast.CallExpr, key string,
	declByObj map[*types.Func]funcDecl) (entry, bool) {

	callee := calleeFunc(st.pass.TypesInfo, call)
	if callee == nil || callee.Pkg() == nil ||
		callee.Pkg().Path() != st.cfg.actorPkg() ||
		len(call.Args) != 1 {
		return entry{}, false
	}

	switch callee.Name() {
	case "NewFunctionBehavior", "FunctionBehaviorFromSimple":
	default:
		return entry{}, false
	}

	name := "function behavior passed to " + callee.Name()

	var id *ast.Ident
	switch arg := call.Args[0].(type) {
	case *ast.FuncLit:
		return entry{name: name, body: arg.Body, key: key}, true

	case *ast.Ident:
		id = arg

	case *ast.SelectorExpr:
		id = arg.Sel

	default:
		return entry{}, false
	}

	// A named function or method value is an entry if its body is in this
	// package.
	fn, _ := st.pass.TypesInfo.Uses[id].(*types.Func)
	d, ok := declByObj[fn]
	if !ok {
		return entry{}, false
	}

	return entry{name: name, body: d.decl.Body, key: d.key}, true
}

// isReceive reports whether fd is an actor Receive method: Receive(ctx, msg)
// or, for a TxBehavior, Receive(ctx, msg, actor.Exec[S]), with a single result,
// where msg implements the actor Message interface.
func (st *pkgState) isReceive(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || fd.Name.Name != "Receive" {
		return false
	}

	obj, _ := st.pass.TypesInfo.Defs[fd.Name].(*types.Func)
	if obj == nil {
		return false
	}

	sig := obj.Type().(*types.Signature)
	if sig.Params().Len() < 2 || sig.Params().Len() > 3 ||
		sig.Results().Len() != 1 ||
		!isContext(sig.Params().At(0).Type()) {
		return false
	}

	var actorPkg *types.Package
	for _, imp := range st.pass.Pkg.Imports() {
		if imp.Path() == st.cfg.actorPkg() {
			actorPkg = imp
		}
	}
	if actorPkg == nil {
		return false
	}

	// A TxBehavior takes the Exec handle as a third parameter.
	if sig.Params().Len() == 3 {
		named, ok := sig.Params().At(2).Type().(*types.Named)
		if !ok || named.Obj().Pkg() != actorPkg ||
			named.Obj().Name() != "Exec" {
			return false
		}
	}

	msgObj := actorPkg.Scope().Lookup("Message")
	if msgObj == nil {
		return false
	}
	iface, ok := msgObj.Type().Underlying().(*types.Interface)
	if !ok {
		return false
	}

	msg := sig.Params().At(1).Type()

	return types.Implements(msg, iface) ||
		types.Implements(types.NewPointer(msg), iface)
}

// recvName returns the receiver type name of a method declaration.
func recvName(fd *ast.FuncDecl) string {
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if idx, ok := t.(*ast.IndexExpr); ok {
		t = idx.X
	}
	if idx, ok := t.(*ast.IndexListExpr); ok {
		t = idx.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}

	return "?"
}

// isExtraEntry reports whether fd is a method named by one of the configured
// entry method specs, on a type that has every method of the spec's
// interface. The interface is matched by method names only, so generic
// interfaces such as protofsm.State need no instantiation.
func (st *pkgState) isExtraEntry(fd *ast.FuncDecl) bool {
	if fd.Recv == nil {
		return false
	}

	obj, _ := st.pass.TypesInfo.Defs[fd.Name].(*types.Func)
	if obj == nil {
		return false
	}
	recv := obj.Type().(*types.Signature).Recv().Type()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}

	for _, spec := range st.cfg.entryMethods() {
		method, ifacePath, ok := strings.Cut(spec, "@")
		dot := strings.LastIndex(ifacePath, ".")
		if !ok || dot < 0 || method != fd.Name.Name {
			continue
		}

		iface := st.lookupInterface(ifacePath[:dot], ifacePath[dot+1:])
		if iface == nil {
			continue
		}

		if hasAllMethods(recv, iface) {
			return true
		}
	}

	return false
}

// lookupInterface finds the named interface in the package being analyzed or
// one of its direct imports.
func (st *pkgState) lookupInterface(pkgPath, name string) *types.Interface {
	pkgs := append([]*types.Package{st.pass.Pkg}, st.pass.Pkg.Imports()...)
	for _, pkg := range pkgs {
		if pkg.Path() != pkgPath {
			continue
		}

		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			return nil
		}

		iface, _ := tn.Type().Underlying().(*types.Interface)

		return iface
	}

	return nil
}

// hasAllMethods reports whether the pointer to t has a method with the name
// and parameter count of every method of iface.
func hasAllMethods(t types.Type, iface *types.Interface) bool {
	set := types.NewMethodSet(types.NewPointer(t))
	for i := range iface.NumMethods() {
		want := iface.Method(i)

		sel := set.Lookup(want.Pkg(), want.Name())
		if sel == nil {
			return false
		}

		got := sel.Obj().Type().(*types.Signature)
		wantSig := want.Type().(*types.Signature)
		if got.Params().Len() != wantSig.Params().Len() {
			return false
		}
	}

	return true
}
