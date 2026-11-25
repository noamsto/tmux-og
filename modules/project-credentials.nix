# True when a declared project remote may embed a secret. The Nix twin of
# credentialBearing in picker/project/registry.go; the two are pinned by
# picker/project/testdata/credential-cases.json.
{lib}: value: let
  plainUserinfo = u: builtins.match "[A-Za-z0-9._-]+" u != null;

  # Mirrors credentialBearing: an `@` is allowed only after a bare username or an
  # ssh/git scheme and bare username, and a `@` in a URL path after an authority is not userinfo.
  parts = lib.splitString "@" value;
  hasAt = builtins.length parts > 1;
  before = builtins.head parts;
  urlParts = lib.splitString "://" before;
  hasScheme = builtins.length urlParts > 1;
  scheme = lib.toLower (builtins.head urlParts);
  rest = lib.concatStringsSep "://" (builtins.tail urlParts);
  authority = builtins.head (lib.splitString "/" rest);
  schemeMayCarryUser = builtins.elem scheme ["ssh" "git" "git+ssh" "ssh+git"];
  refused =
    if !hasAt
    then false
    else if plainUserinfo before
    then builtins.length parts > 2
    else if !hasScheme
    then true
    else if lib.hasInfix "/" rest
    then authority == "" || lib.hasInfix ":" rest
    else !(schemeMayCarryUser && plainUserinfo rest) || builtins.length parts > 2;
in
  lib.hasInfix "?" value
  || lib.hasInfix "#" value
  || refused
