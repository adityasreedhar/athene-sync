--[[--
Talks to the Athene sync server: the same account as KOReader's Progress sync, and the routes
under /athene/v1 that keep books, highlights, notes and bookmarks.

Every call is synchronous and short; a book's file goes straight to disk.
]]

local http = require("socket.http")
local ltn12 = require("ltn12")
local rapidjson = require("rapidjson")
local socket = require("socket")
local socketutil = require("socketutil")

local Client = {}
Client.__index = Client

--- account: { server, username, userkey, device, device_id, version }
function Client.new(account)
    return setmetatable({ account = account }, Client)
end

local function percentEncode(s)
    return (tostring(s or ""):gsub("[^%w%-%._~]", function(c)
        return string.format("%%%02X", string.byte(c))
    end))
end
Client.percentEncode = percentEncode

-- rapidjson hands JSON's null back as a value of its own; nothing here wants it.
local function stripNulls(v)
    if v == rapidjson.null then return nil end
    if type(v) == "table" then
        for k, x in pairs(v) do
            if x == rapidjson.null then
                v[k] = nil
            elseif type(x) == "table" then
                stripNulls(x)
            end
        end
    end
    return v
end

function Client:headers(extra)
    local a = self.account
    local h = {
        ["x-auth-user"] = a.username,
        ["x-auth-key"] = a.userkey,
        ["X-Athene-Device"] = percentEncode(a.device),
        ["X-Athene-Device-Id"] = percentEncode(a.device_id),
        ["X-Athene-Client"] = "koreader-plugin/" .. tostring(a.version or "?"),
        ["Accept"] = "application/json",
    }
    for k, v in pairs(extra or {}) do h[k] = v end
    return h
end

local function requester(url)
    if url:match("^https:") then
        local ok, https = pcall(require, "ssl.https")
        if ok then return https end
    end
    return http
end

--- A JSON call. Returns status, decoded body; or nil, "network" when nothing answered.
function Client:call(method, path, body, timeouts)
    local url = self.account.server .. path
    local headers = self:headers()
    local source
    if body ~= nil then
        local data = rapidjson.encode(body)
        headers["Content-Type"] = "application/json"
        headers["Content-Length"] = tostring(#data)
        source = ltn12.source.string(data)
    end
    local chunks = {}
    timeouts = timeouts or {}
    socketutil:set_timeout(timeouts[1] or socketutil.LARGE_BLOCK_TIMEOUT, timeouts[2] or socketutil.LARGE_TOTAL_TIMEOUT)
    local code, resp_headers = socket.skip(1, requester(url).request{
        url = url,
        method = method,
        headers = headers,
        source = source,
        sink = socketutil.table_sink(chunks),
    })
    socketutil:reset_timeout()
    if resp_headers == nil or type(code) ~= "number" then
        return nil, "network"
    end
    local text = table.concat(chunks)
    local decoded
    if text ~= "" then
        local ok, value = pcall(rapidjson.decode, text)
        if ok then decoded = stripNulls(value) end
    end
    return code, decoded
end

--- Saves a response body to `path` (written beside it, then moved into place).
function Client:download(path_on_server, path)
    local url = self.account.server .. path_on_server
    local part = path .. ".part"
    local handle = io.open(part, "wb")
    if not handle then return false, "write" end
    socketutil:set_timeout(30, -1)
    local code, resp_headers = socket.skip(1, requester(url).request{
        url = url,
        method = "GET",
        headers = self:headers(),
        sink = socketutil.file_sink(handle),
    })
    socketutil:reset_timeout()
    if resp_headers == nil or code ~= 200 then
        os.remove(part)
        return false, resp_headers == nil and "network" or code
    end
    os.remove(path)
    local ok = os.rename(part, path)
    if not ok then
        os.remove(part)
        return false, "write"
    end
    return true
end

--- Whether the server is an Athene server, and what it says of itself.
function Client:info()
    local code, body = self:call("GET", "/athene/v1/info", nil, { 5, 10 })
    if code == 200 and type(body) == "table" and body.server == "athene-sync" then
        return body
    end
    if code == nil then return nil, nil end   -- nothing answered
    return nil, 404                           -- something answered, but not an Athene server
end

--- Sends what changed here, and gets what changed since `since`.
function Client:syncAnnotations(document, since, records, timeouts)
    return self:call("POST", "/athene/v1/annotations/" .. document,
        { since = since or 0, annotations = rapidjson.array(records or {}) }, timeouts or { 10, 30 })
end

function Client:getAnnotations(document, since)
    return self:call("GET", "/athene/v1/annotations/" .. document .. "?since=" .. tostring(since or 0), nil, { 10, 30 })
end

function Client:books()
    return self:call("GET", "/athene/v1/books", nil, { 10, 30 })
end

function Client:delivered(id)
    return self:call("POST", "/athene/v1/books/" .. id .. "/delivered",
        { device = self.account.device, device_id = self.account.device_id }, { 10, 20 })
end

--- KOReader's Progress sync route, for the place a new book should open at.
function Client:progress(document)
    local url = self.account.server .. "/syncs/progress/" .. document
    local chunks = {}
    socketutil:set_timeout(5, 10)
    local code, resp_headers = socket.skip(1, requester(url).request{
        url = url,
        method = "GET",
        headers = self:headers({ ["Accept"] = "application/vnd.koreader.v1+json" }),
        sink = socketutil.table_sink(chunks),
    })
    socketutil:reset_timeout()
    if resp_headers == nil or code ~= 200 then return nil end
    local ok, value = pcall(rapidjson.decode, table.concat(chunks))
    if ok and type(value) == "table" then return stripNulls(value) end
end

function Client:pluginInfo()
    return self:call("GET", "/athene/v1/plugin", nil, { 5, 15 })
end

return Client
