# True when a declared project remote may embed a secret. The Nix twin of
# credentialBearing in picker/project/registry.go; the two are pinned by
# picker/project/testdata/credential-cases.json.
{lib}: value: let
  plainUserinfo = u: builtins.match "[A-Za-z0-9._-]+" u != null;

  # Everything before the last `@`, or null without one.
  userinfoOf = authority: let
    parts = lib.splitString "@" authority;
  in
    if builtins.length parts < 2
    then null
    else lib.concatStringsSep "@" (lib.init parts);

  # `builtins.split` cuts at every `://`; the first piece is the scheme and the next
  # (up to its own first `/`) the authority.
  schemeParts = builtins.split "://" value;
  hasScheme = builtins.length schemeParts > 1;
  scheme = lib.toLower (builtins.elemAt schemeParts 0);
  firstSegment = s: builtins.head (lib.splitString "/" s);
  authority =
    if hasScheme
    then firstSegment (builtins.elemAt schemeParts 2)
    else firstSegment value;
  userinfo = userinfoOf authority;
  schemeMayCarryUser = builtins.elem scheme ["ssh" "git" "git+ssh" "ssh+git"];
in
  lib.hasInfix "?" value
  || lib.hasInfix "#" value
  || (userinfo != null && !((!hasScheme || schemeMayCarryUser) && plainUserinfo userinfo))
