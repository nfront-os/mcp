# scripts/nfront-paths.sh: where nfront's parts are, for a repository
# beside the 9front tree (9front-gcc, 9front-rust, 9front-go, 9front-mcp).
# Sourced after TOP (this repository) is set; exports each variable of the
# distribution's paths.env (TREE, TREE_GCC, TREE_NIX, TREE_RISCV, GCC_DIR,
# RUST_DIR, GO_DIR, ...) that the environment does not set already, as an
# absolute path.  paths.env: $NFRONT_PATHS, else TOP/../dist/paths.env or TOP/../../dist/paths.env (the
# umbrella's layout; ../../dist for the toolchains in toolchain/), else TOP/../9front-distribution/paths.env; without it
# the old layout's ../9front-* names.  The same file in each repository.
__nfront_paths(){
	local f top line k v
	for f in "$NFRONT_PATHS" "$TOP/../dist/paths.env" "$TOP/../../dist/paths.env" "$TOP/../9front-distribution/paths.env"; do
		[ -n "$f" ] && [ -f "$f" ] && break
		f=
	done
	if [ -n "$f" ]; then
		top=$(cd -P "$(dirname "$f")/.." && pwd -P)	# physical: through a symlink of the old layout too
		while IFS= read -r line; do
			case "$line" in ''|'#'*) continue;; esac
			k=${line%%=*}
			v=${line#*=}
			case "$v" in /*) ;; *) v=$top/$v;; esac
			[ -z "${!k}" ] && export "$k=$v"
		done < "$f"
	fi
	top=$(cd "$TOP/.." && pwd)
	: "${TREE:=$top/9front}" "${TREE_GCC:=$top/9front-gccwt}" "${TREE_NIX:=$top/9front-nix}"
	: "${TREE_RISCV:=$top/9front-riscv}" "${GCC_DIR:=$top/9front-gcc}" "${RUST_DIR:=$top/9front-rust}"
	: "${GO_DIR:=$top/9front-go}" "${MCP_DIR:=$top/9front-mcp}"
	export TREE TREE_GCC TREE_NIX TREE_RISCV GCC_DIR RUST_DIR GO_DIR MCP_DIR
}
__nfront_paths
