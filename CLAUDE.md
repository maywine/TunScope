# Repository Constraints

- Windows Store/MSIX applications must be selected by `PackageFamilyName`, not by a versioned `WindowsApps` path or a broad path wildcard.
- Windows per-application routing must keep absolute executable-path compatibility and apply both path and package-family matches to descendant processes.
