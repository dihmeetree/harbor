local core = require("apisix.core")

-- Declare the plugin's name
local plugin_name = "hello"

-- Define the plugin schema
local plugin_schema = {
    type = "object",
    properties = {},
    required = {},
}

-- Define the plugin with its version, priority, name, and schema
local _M = {
    version = 0.1,
    priority = 2000,
    name = plugin_name,
    schema = plugin_schema
}

-- Function to check if the plugin configuration is correct
function _M.check_schema(conf)
    return core.schema.check(plugin_schema, conf)
end

-- Function to be called during the access phase
function _M.access(conf, ctx)
    core.response.exit(200, { message = "hit hello plugin!" })
end

-- Return the plugin so it can be used by APISIX
return _M
