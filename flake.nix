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

      nixosModules.default = import ./nix/module.nix self;

      formatter.${system} = pkgs.nixfmt;

      checks.${system} = {
        inherit package;

        # Evaluates the module in a minimal nixosSystem and forces the unit
        # definition (incl. the rendered YAML config) without building a
        # full system closure.
        module-eval =
          let
            eval = nixpkgs.lib.nixosSystem {
              inherit system;
              modules = [
                self.nixosModules.default
                {
                  services.github-qemu-runner = {
                    enable = true;
                    privateKeyFile = "/etc/github-qemu-runner/app-key.pem";
                    settings = {
                      github = {
                        app_id = 1;
                        installation_id = 1;
                      };
                      pools = [
                        {
                          name = "fmt";
                          scope = "org";
                          org = "test";
                          count = 1;
                          cpus = 1;
                          memory_mb = 512;
                          disk_gb = 20;
                          labels = [ "self-hosted" ];
                        }
                      ];
                    };
                  };
                  system.stateVersion = "25.05";
                }
              ];
            };
          in
          pkgs.runCommand "github-qemu-runner-module-eval"
            {
              execStart = eval.config.systemd.services.github-qemu-runner.serviceConfig.ExecStart;
              user = eval.config.users.users.gh-runner.name;
            }
            ''
              echo "$user: $execStart" > "$out"
            '';
      };
    };
}
