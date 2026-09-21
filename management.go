package main

// managementBasePath is the Management API base path. Data endpoints are registered
// underneath it, which is the only place the plugin exposes data - the host
// authenticates every Management API request.
const managementBasePath = "/v0/management/plugins/" + pluginID
