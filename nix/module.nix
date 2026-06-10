# NixOS module. Imported via the flake's nixosModules.default, which passes
# `self` so the package option can default to the flake's own build.
self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.github-qemu-runner;
  settingsFormat = pkgs.formats.yaml { };
  configFile = settingsFormat.generate "github-qemu-runner.yaml" cfg.settings;
in
{
  options.services.github-qemu-runner = {
    enable = lib.mkEnableOption "ephemeral GitHub Actions runners on QEMU/KVM";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "github-qemu-runner from this flake";
      description = "github-qemu-runner package to run.";
    };

    settings = lib.mkOption {
      type = settingsFormat.type;
      default = { };
      description = ''
        Configuration rendered to the YAML config file; same schema as
        packaging/config.example.yaml in the repository.
        github.private_key_path defaults to the systemd LoadCredential
        path and normally should not be overridden.
      '';
    };

    privateKeyFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        GitHub App private key, passed to the service via systemd
        LoadCredential. Use a string path (e.g. "/run/secrets/app-key.pem"),
        not a Nix path literal — a literal would copy the key into the
        world-readable store. Note CREDENTIALS_DIRECTORY exists only inside
        the service; run `setup`/`refresh-image` manually via
        `systemd-run -P --wait -p LoadCredential=app-key.pem:<path> ...`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    services.github-qemu-runner.settings.github.private_key_path =
      lib.mkDefault "\${CREDENTIALS_DIRECTORY}/app-key.pem";

    users.users.gh-runner = {
      isSystemUser = true;
      group = "gh-runner";
      extraGroups = [ "kvm" ];
      home = "/var/lib/github-qemu-runner";
    };
    users.groups.gh-runner = { };

    systemd.services.github-qemu-runner = {
      description = "Ephemeral GitHub Actions runners on QEMU/KVM";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      # qemu-system-x86_64 / qemu-img / genisoimage for the controller
      path = [
        pkgs.qemu_kvm
        pkgs.cdrkit
      ];
      serviceConfig = {
        Type = "exec";
        User = "gh-runner";
        Group = "gh-runner";
        SupplementaryGroups = [ "kvm" ];
        ExecStart = "${lib.getExe cfg.package} -config ${configFile} controller";
        Restart = "on-failure";
        RestartSec = 10;
        # Must exceed the largest pool drain_timeout (default 30m).
        TimeoutStopSec = "35min";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        StateDirectory = "github-qemu-runner";
        ReadWritePaths = [ "/var/lib/github-qemu-runner" ];
        LoadCredential = [ "app-key.pem:${cfg.privateKeyFile}" ];
      };
    };
  };
}
