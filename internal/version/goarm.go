package version

// goarm is set at link time for GOARCH=arm builds (-X …/version.goarm=7);
// the runtime does not expose GOARM.
var goarm = "v7"
