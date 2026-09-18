{
  description = "docker-extras dev shell";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  # Dev shell only — plain bash ships as source, so there is nothing to
  # build. The shell pins the release/hygiene toolchain; CI stays on the
  # ubuntu-latest host tools (shellcheck and a docker daemon are already
  # there).
  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        devShells.default = pkgs.mkShell {
          name = "docker-extras";
          packages = with pkgs; [
            shellcheck
            git-cliff
            gh
            gnumake
            pre-commit
          ];
          # stderr, not stdout: anything this hook prints to stdout lands
          # in front of piped command output.
          shellHook = ''
            echo "docker-extras dev shell — run 'make check' to lint+test, 'make hooks' to install pre-commit." >&2
          '';
        };
      });
}
