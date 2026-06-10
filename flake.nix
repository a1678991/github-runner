{
  description = "Ephemeral GitHub Actions self-hosted runners on QEMU/KVM";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      package = pkgs.callPackage ./nix/package.nix { src = self; };
    in
    {
      packages.${system}.default = package;

      formatter.${system} = pkgs.nixfmt;

      checks.${system} = {
        inherit package;
      };
    };
}
