--[[--
Athene: keeps KOReader in step with Athene on an iPhone or iPad, through the reader's own Athene
sync server.

- Highlights, notes and bookmarks go both ways, by themselves: when a book opens, a moment after
  one changes, when it closes, and when the e-reader goes to sleep or wakes on Wi-Fi.
- Books sent from Athene arrive when the e-reader wakes on Wi-Fi (or on Sync now), in a folder of
  their own, already holding their highlights and opening at the place reached in Athene.
- The place itself is KOReader's Progress sync's, which the same server answers.

The account is Progress sync's: the server, name and key it signed in with. Nothing else is asked.
This plugin never turns Wi-Fi on by itself; Sync now does, as KOReader's own syncs do.

Copyright (C) 2026 Aditya Sreedhar. Free software under the GNU Affero General Public License,
version 3.
]]

local BD = require("ui/bidi")
local DataStorage = require("datastorage")
local Device = require("device")
local Dispatcher = require("dispatcher")
local DocSettings = require("docsettings")
local Event = require("ui/event")
local InfoMessage = require("ui/widget/infomessage")
local LuaSettings = require("luasettings")
local NetworkMgr = require("ui/network/manager")
local Notification = require("ui/widget/notification")
local UIManager = require("ui/uimanager")
local WidgetContainer = require("ui/widget/container/widgetcontainer")
local ffiutil = require("ffi/util")
local lfs = require("libs/libkoreader-lfs")
local logger = require("logger")
local md5 = require("ffi/sha2").md5
local util = require("util")
local _ = require("gettext")
local N_ = _.ngettext
local T = ffiutil.template

local Client = require("athene_client")
local Records = require("athene_records")

-- Athene's menu sits right under Progress sync on the Tools tab, whose account it shares. Left to
-- itself KOReader puts a plugin with no place in its menu order after More tools, which on most
-- e-readers is the Tools tab's second page, where it went unfound (2026-10-05). The file browser
-- has no Progress sync, so there Athene takes the same place, after Reading statistics, and is on
-- the first page there too. Done once, as the plugin loads and before any menu is sorted; a
-- KOReader whose order lacks all of these keeps its own placement.
local function placeMenu(order_module)
    local ok, order = pcall(require, order_module)
    if not ok or type(order) ~= "table" or type(order.tools) ~= "table" then return end
    local tools = order.tools
    for _, id in ipairs(tools) do
        if id == "athene" then return end
    end
    for _, after in ipairs({ "progress_sync", "statistics" }) do
        for i, id in ipairs(tools) do
            if id == after then
                table.insert(tools, i + 1, "athene")
                return
            end
        end
    end
    for i, id in ipairs(tools) do
        if id == "more_tools" then
            local above = (i > 1 and type(tools[i - 1]) == "string" and tools[i - 1]:match("^%-+$")) and i - 1 or i
            table.insert(tools, above, "athene")
            return
        end
    end
end
placeMenu("ui/elements/reader_menu_order")
placeMenu("ui/elements/filemanager_menu_order")

local Athene = WidgetContainer:extend{
    name = "athene",
    is_doc_only = false,
}

local SETTINGS_KEY = "athene_sync"   -- in KOReader's own settings: this plugin's choices
local STATE_KEY = "athene_sync"      -- in each book's settings: what is in step with the server
local PUSH_DELAY = 8                 -- seconds after a change before it goes
local QUICK = { 3, 10 }              -- timeouts when nobody asked: a sync must not hold the page
local PAGED = { pdf = true, djvu = true, cbz = true, cbr = true, cbt = true, xps = true, oxps = true }

function Athene:init()
    self.settings = G_reader_settings:readSetting(SETTINGS_KEY, {})
    if self.settings.auto_sync == nil then
        self.settings.auto_sync = true
    end
    self.device_id = G_reader_settings:readSetting("device_id")
    self.push_task = function() self:syncBook(false) end
    self.wake_task = function() self:onWoke() end
    self:onDispatcherRegisterActions()
    self.ui.menu:registerToMainMenu(self)
end

function Athene:onDispatcherRegisterActions()
    Dispatcher:registerAction("athene_sync", { category = "none", event = "AtheneSync", title = _("Sync with Athene"), general = true })
end

function Athene:saveSettings()
    G_reader_settings:saveSetting(SETTINGS_KEY, self.settings)
end

-- ---- the account ---------------------------------------------------------------------------------

-- Progress sync's settings: from the running plugin when a book is open, otherwise from its file
-- (KOReader 2025 and later), or KOReader's own settings (before that).
function Athene:kosyncSettings()
    if self.ui.kosync and type(self.ui.kosync.settings) == "table" then
        return self.ui.kosync.settings
    end
    local file = DataStorage:getSettingsDir() .. "/kosync.lua"
    if lfs.attributes(file, "mode") == "file" then
        local ok, settings = pcall(function() return LuaSettings:open(file):readSetting("settings") end)
        if ok and type(settings) == "table" then return settings end
    end
    return G_reader_settings:readSetting("kosync") or {}
end

function Athene:account()
    local ko = self:kosyncSettings()
    if type(ko.custom_server) ~= "string" or ko.custom_server == "" or not ko.username or not ko.userkey then
        return nil
    end
    local server = ko.custom_server:gsub("%s", ""):gsub("/+$", "")
    if not server:match("^https?://") then
        server = "http://" .. server
    end
    return {
        server = server,
        username = ko.username,
        userkey = ko.userkey,
        device = ko.kosync_hostname or Device.model,
        device_id = self.device_id,
        version = self.version,
        by_file_name = ko.checksum_method == 1,
    }
end

function Athene:serverName(account)
    return (account.server:gsub("^https?://", ""))
end

function Athene:explainSignIn()
    UIManager:show(InfoMessage:new{
        text = _("Sign in to your Athene server first: open a book, then Tools ▸ Progress sync ▸ Custom sync server, and choose Register / Login. Athene uses the same account."),
    })
end

-- What a failed call means, in a sentence.
function Athene:problemText(account, code)
    if code == nil then
        return T(_("Couldn't reach your Athene server at %1."), self:serverName(account))
    elseif code == 401 then
        return _("Your Athene server didn't accept the name and password. Log in again in Tools ▸ Progress sync.")
    elseif code == 404 then
        return T(_("%1 answers, but it isn't an Athene sync server. KOReader's own sync server keeps only your place; point Progress sync at your Athene server."), self:serverName(account))
    end
    return T(_("Your Athene server couldn't sync (%1)."), tostring(code))
end

-- ---- a book's highlights, notes and bookmarks ---------------------------------------------------------

function Athene:isPaging()
    return self.ui.paging and true or false
end

-- KOReader's name for the open book, by the matching Progress sync uses.
function Athene:documentID(account)
    if account.by_file_name then
        local file = self.ui.document and self.ui.document.file
        if not file then return end
        local _dir, name = util.splitFilePathName(file)
        return name and md5(name)
    end
    return self.ui.doc_settings and self.ui.doc_settings:readSetting("partial_md5_checksum")
end

function Athene:indexOf(id)
    for i, item in ipairs(self.ui.annotation.annotations) do
        if item.athene_id == id then return i end
    end
end

-- Whether this book can show a record's place.
function Athene:placeable(item)
    local doc = self.ui.document
    if self:isPaging() then
        local count = doc:getPageCount()
        local function ok(page) return type(page) == "number" and page >= 1 and page <= count end
        if item.drawer then
            return ok(item.page) and ok(item.pos0.page) and ok(item.pos1.page)
        end
        return ok(item.page)
    end
    if item.drawer then
        return doc:isXPointerInDocument(item.pos0) and doc:isXPointerInDocument(item.pos1)
    end
    return doc:isXPointerInDocument(item.page)
end

function Athene:addItem(item)
    local index = self.ui.annotation:addItem(item)
    if item.drawer then
        local count = item.note and { nb_notes_added = 1 } or { nb_highlights_added = 1 }
        self.ui:handleEvent(Event:new("AnnotationsModified",
            { item, nb_highlights_added = count.nb_highlights_added, nb_notes_added = count.nb_notes_added, index_modified = index }))
    else
        self.ui:handleEvent(Event:new("AnnotationsModified", { item, index_modified = index }))
    end
end

function Athene:removeAt(index)
    local item = self.ui.annotation.annotations[index]
    if item.drawer then
        self.ui.highlight:deleteHighlight(index)
    else
        table.remove(self.ui.annotation.annotations, index)
        self.ui:handleEvent(Event:new("AnnotationsModified", { item, index_modified = -index }))
    end
end

function Athene:updateItem(item, rec)
    item.text = rec.text or item.text
    item.chapter = rec.chapter or item.chapter
    if item.drawer then
        item.note = type(rec.note) == "string" and rec.note ~= "" and rec.note or nil
        item.color = rec.color or item.color
        item.drawer = rec.drawer or item.drawer
    end
    self.ui:handleEvent(Event:new("AnnotationsModified", { item }))
    -- KOReader dates an edit now; this one was made when the record says.
    item.datetime_updated = Records.datetime(rec.updated)
end

--[[
Takes what the server says changed: this device's own changes come back too, and change nothing.
Returns how many annotations were added, changed and removed here.
]]
function Athene:applyRecords(records, state)
    local paging = self:isPaging()
    local added, changed, removed = 0, 0, 0
    for _, rec in ipairs(records) do
        local id = rec.id
        local index = type(id) == "string" and self:indexOf(id)
        local item = index and self.ui.annotation.annotations[index]
        local updated = tonumber(rec.updated) or 0
        if type(id) ~= "string" then
            -- not a record
        elseif rec.deleted then
            if item and Records.itemStamp(item) <= updated then
                self:removeAt(index)
                removed = removed + 1
            end
            state.known[id] = nil
        elseif item then
            if updated > Records.itemStamp(item) and not Records.sameContent(item, rec) then
                local moved = Records.placeKey(item) ~= Records.placeKey(rec)
                local fresh = moved and Records.toItem(rec, paging)
                if moved and fresh and self:placeable(fresh) then
                    self:removeAt(index)
                    self:addItem(fresh)
                    changed = changed + 1
                elseif not moved then
                    self:updateItem(item, rec)
                    changed = changed + 1
                end
            end
            state.known[id] = math.max(state.known[id] or 0, updated)
        else
            local fresh = Records.toItem(rec, paging)
            if fresh and self:placeable(fresh) then
                -- The same place highlighted here too, before the two met: one annotation, the
                -- record's id; the id it had here is let go of.
                local twin
                local key = Records.placeKey(fresh)
                for i, other in ipairs(self.ui.annotation.annotations) do
                    if Records.placeKey(other) == key and other.athene_id ~= id then
                        twin = i
                        break
                    end
                end
                if twin then
                    -- (Its old id stays known and is no longer here, so the next sync lets it go.)
                    local other = self.ui.annotation.annotations[twin]
                    other.athene_id = id
                    if updated >= Records.itemStamp(other) and not Records.sameContent(other, rec) then
                        self:updateItem(other, rec)
                        changed = changed + 1
                    end
                else
                    self:addItem(fresh)
                    added = added + 1
                end
                state.known[id] = updated
            end
        end
    end
    return added, changed, removed
end

-- What an annotation says, as one string: a note edited in KOReader changes no date, only this.
local function signature(item)
    return table.concat({ Records.placeKey(item), item.note or "", item.color or "", item.drawer or "", item.text or "" }, "\1")
end

function Athene:bookState(document)
    local state = self.ui.doc_settings:readSetting(STATE_KEY)
    if type(state) ~= "table" or state.document ~= document then
        state = { document = document, rev = 0, known = {} }
    end
    state.known = state.known or {}
    state.sig = state.sig or {}
    return state
end

--[[
What changed here since the last sync: annotations new, edited or removed. Gives each new one its
id, and dates one KOReader changed without dating it. Returns the records to send, their stamps
by id, and the ids present.
]]
function Athene:localChanges(state, account)
    local outgoing, sent, present = {}, {}, {}
    local now = Records.now()
    for _, item in ipairs(self.ui.annotation.annotations) do
        if not item.athene_id then
            item.athene_id = Records.newID(item)
        end
        local id = item.athene_id
        present[id] = true
        local known = state.known[id]
        local stamp = Records.itemStamp(item)
        local edited = known and state.sig[id] and state.sig[id] ~= signature(item)
        if not known or stamp > known or edited then
            if edited and stamp <= known then
                item.datetime_updated = Records.datetime(now)
            end
            local rec = Records.fromItem(item, account.device, account.device_id)
            if edited and rec.updated <= known then rec.updated = now end
            table.insert(outgoing, rec)
            sent[id] = rec.updated
        end
    end
    for id in pairs(state.known) do
        if not present[id] then
            table.insert(outgoing, { id = id, updated = now, deleted = true })
            sent[id] = now
        end
    end
    return outgoing, sent, present
end

-- Whether this book has anything to send, worked out without the network.
function Athene:hasLocalChanges()
    if not (self.ui.document and self.ui.annotation and self.ui.annotation.annotations and self.ui.doc_settings) then
        return false
    end
    local account = self:account()
    local document = account and self:documentID(account)
    if not document then return false end
    local state = self:bookState(document)
    for _, item in ipairs(self.ui.annotation.annotations) do
        local id = item.athene_id
        local known = id and state.known[id]
        if not known or Records.itemStamp(item) > known or (state.sig[id] and state.sig[id] ~= signature(item)) then
            return true
        end
    end
    local present = {}
    for _, item in ipairs(self.ui.annotation.annotations) do
        if item.athene_id then present[item.athene_id] = true end
    end
    for id in pairs(state.known) do
        if not present[id] then return true end
    end
    return false
end

--[[
Sends this book's changes and takes the server's, in one call. `interactive`: someone asked, so
say what went wrong and wait longer. Returns the counts applied, or nil.
]]
function Athene:syncBook(interactive, timeouts)
    if not (self.ui.document and self.ui.annotation and self.ui.annotation.annotations and self.ui.doc_settings) then
        return
    end
    local account = self:account()
    if not account then
        if interactive then self:explainSignIn() end
        return
    end
    local document = self:documentID(account)
    if not document then return end

    local state = self:bookState(document)
    local outgoing, sent, present = self:localChanges(state, account)

    local client = Client.new(account)
    local code, body = client:syncAnnotations(document, state.rev, outgoing, timeouts)
    if code ~= 200 or type(body) ~= "table" then
        logger.warn("Athene: sync failed", code)
        if interactive then
            UIManager:show(InfoMessage:new{ text = self:problemText(account, code) })
        end
        return nil, true
    end
    for id, stamp in pairs(sent) do
        if present[id] then state.known[id] = stamp else state.known[id] = nil end
    end

    self.applying = true
    local ok, added, changed, removed = pcall(self.applyRecords, self, body.annotations or {}, state)
    self.applying = false
    if not ok then
        logger.warn("Athene: couldn't apply what the server sent:", added)
        return nil, true
    end
    state.rev = tonumber(body.rev) or state.rev
    state.sig = {}
    for _, item in ipairs(self.ui.annotation.annotations) do
        if item.athene_id and state.known[item.athene_id] then
            state.sig[item.athene_id] = signature(item)
        end
    end
    self.ui.doc_settings:saveSetting(STATE_KEY, state)
    self.settings.last_sync = os.time()
    self:saveSettings()
    if added + changed + removed > 0 then
        UIManager:setDirty(self.dialog, "ui")
        if self.ui.bookmark and self.ui.bookmark.setDogearVisibility then
            self.ui.bookmark:setDogearVisibility(self.ui.bookmark:getCurrentPageNumber())
        end
    end
    logger.dbg("Athene: synced", document, "sent", #outgoing, "added", added, "changed", changed, "removed", removed)
    return { added = added, changed = changed, removed = removed }
end

function Athene:onAnnotationsModified()
    if self.applying then return end
    if self.settings.auto_sync and NetworkMgr:isOnline() then
        UIManager:unschedule(self.push_task)
        UIManager:scheduleIn(PUSH_DELAY, self.push_task)
    end
end

function Athene:onReaderReady()
    if self.settings.auto_sync then
        UIManager:scheduleIn(1, function()
            if NetworkMgr:isOnline() then
                self:syncBook(false, QUICK)
            end
        end)
    end
end

-- Closing and sleeping send what changed, notes edited in place included (KOReader says nothing
-- when a note's words change), and skip the network when nothing did.
function Athene:onCloseDocument()
    UIManager:unschedule(self.push_task)
    if self.settings.auto_sync and NetworkMgr:isOnline() and self:hasLocalChanges() then
        self:syncBook(false, QUICK)
    end
end

function Athene:onSuspend()
    UIManager:unschedule(self.push_task)
    UIManager:unschedule(self.wake_task)
    if self.ui.document and self.settings.auto_sync and NetworkMgr:isOnline() and self:hasLocalChanges() then
        self:syncBook(false, QUICK)
    end
end

function Athene:onResume()
    if self.settings.auto_sync then
        UIManager:unschedule(self.wake_task)
        UIManager:scheduleIn(3, self.wake_task)
    end
end

function Athene:onNetworkConnected()
    if self.settings.auto_sync then
        UIManager:unschedule(self.wake_task)
        UIManager:scheduleIn(2, self.wake_task)
    end
end

function Athene:onCloseWidget()
    UIManager:unschedule(self.push_task)
    UIManager:unschedule(self.wake_task)
end

-- Waking on Wi-Fi: this book's changes, then any books waiting.
function Athene:onWoke()
    if not NetworkMgr:isOnline() or not self:account() then return end
    if self.ui.document then
        self:syncBook(false, QUICK)
    end
    local result = self:fetchBooks(false)
    if result and result.arrived > 0 then
        Notification:notify(self:summary(nil, result))
    end
    self:checkForUpdate(false)
end

-- ---- books from Athene -------------------------------------------------------------------------------

function Athene:booksFolder()
    if type(self.settings.folder) == "string" and self.settings.folder ~= "" then
        return self.settings.folder
    end
    local home = G_reader_settings:readSetting("home_dir") or Device.home_dir or DataStorage:getDataDir()
    return home .. "/Athene"
end

local function extensionOf(name)
    return (name:match("%.([%w]+)$") or ""):lower()
end

local function latestDomVersion()
    local ok, version = pcall(function()
        return require("document/credocument"):engineInit().getLatestDomVersion()
    end)
    if ok and type(version) == "number" then return version end
    return 20240114
end

-- A free place for the book in the folder: its own name, or "Name (2).epub" beside another file.
local function placeFor(folder, filename, document)
    local base, ext = filename:match("^(.*)(%.[^.]+)$")
    if not base then base, ext = filename, "" end
    for n = 1, 50 do
        local name = n == 1 and filename or string.format("%s (%d)%s", base, n, ext)
        local path = folder .. "/" .. name
        if lfs.attributes(path, "mode") ~= "file" then return path, false end
        if util.partialMD5(path) == document then return path, true end
    end
end

-- The new book's KOReader settings, written before it is first opened: its highlights, notes and
-- bookmarks, the place reached in Athene, and what is in step, so it opens there with them.
function Athene:prepareBook(client, path, book)
    local doc_settings = DocSettings:open(path)
    if doc_settings:readSetting("annotations") ~= nil then
        return   -- KOReader has kept settings for this file before: the first opening syncs it
    end
    local paging = PAGED[extensionOf(book.filename)] or false
    local items, known, rev = {}, {}, 0
    local code, body = client:getAnnotations(book.document, 0)
    if code == 200 and type(body) == "table" then
        rev = tonumber(body.rev) or 0
        for _, rec in ipairs(body.annotations or {}) do
            local item = Records.toItem(rec, paging)
            if item then
                table.insert(items, item)
                known[rec.id] = tonumber(rec.updated)
            end
        end
    end
    doc_settings:saveSetting("annotations", items)
    if #items > 0 then
        -- KOReader numbers their pages and puts them in order when the book opens.
        doc_settings:saveSetting("annotations_externally_modified", true)
    end
    doc_settings:saveSetting(STATE_KEY, { document = book.document, rev = rev, known = known })
    doc_settings:saveSetting("partial_md5_checksum", book.document)
    local place = client:progress(book.document)
    if place and place.progress then
        if paging then
            doc_settings:saveSetting("last_page", tonumber(place.progress))
        else
            doc_settings:saveSetting("last_xpointer", place.progress)
            -- A book never opened takes crengine's newest rendering; said here, or KOReader
            -- takes a place it didn't make for an old book's and offers to upgrade it.
            doc_settings:saveSetting("cre_dom_version", latestDomVersion())
        end
        if tonumber(place.percentage) then
            doc_settings:saveSetting("percent_finished", tonumber(place.percentage))
        end
    end
    doc_settings:flush()
end

--[[
Fetches the books Athene sent that this e-reader hasn't got. Returns { arrived, failed }, or nil
when the server couldn't be asked.
]]
function Athene:fetchBooks(interactive)
    local account = self:account()
    if not account then return end
    local client = Client.new(account)
    local code, body = client:books()
    if code ~= 200 or type(body) ~= "table" then
        if interactive then
            UIManager:show(InfoMessage:new{ text = self:problemText(account, code) })
        end
        return
    end
    local waiting = {}
    for _, book in ipairs(body.books or {}) do
        local here = false
        for _, delivery in ipairs(book.deliveries or {}) do
            if delivery.device_id == account.device_id then here = true end
        end
        if not here and type(book.id) == "string" and type(book.filename) == "string" then
            table.insert(waiting, book)
        end
    end
    local result = { arrived = 0, failed = 0, titles = {} }
    if #waiting == 0 then return result end

    local folder = self:booksFolder()
    util.makePath(folder)
    local note = InfoMessage:new{ text = T(N_("Getting a book from Athene…", "Getting %1 books from Athene…", #waiting), #waiting) }
    UIManager:show(note)
    UIManager:forceRePaint()
    for _, book in ipairs(waiting) do
        local path, have = placeFor(folder, book.filename, book.document)
        local ok = have
        if path and not have then
            ok = client:download("/athene/v1/books/" .. book.id .. "/file", path)
            if ok and util.partialMD5(path) ~= book.document then
                os.remove(path)
                ok = false
            end
            if ok then
                local prepared, err = pcall(self.prepareBook, self, client, path, book)
                if not prepared then logger.warn("Athene: preparing", path, err) end
                result.arrived = result.arrived + 1
                table.insert(result.titles, book.title or book.filename)
            end
        end
        if ok then
            client:delivered(book.id)
        else
            result.failed = result.failed + 1
        end
    end
    UIManager:close(note)
    if result.arrived > 0 and self.ui.file_chooser then
        self.ui.file_chooser:refreshPath()
    end
    self.settings.last_sync = os.time()
    self:saveSettings()
    return result
end

-- ---- what was done, in a sentence --------------------------------------------------------------------

function Athene:summary(counts, books)
    local parts = {}
    if books and books.arrived > 0 then
        if books.arrived == 1 and books.titles[1] then
            table.insert(parts, T(_("%1 arrived"), BD.auto(books.titles[1])))
        else
            table.insert(parts, T(N_("1 new book", "%1 new books", books.arrived), books.arrived))
        end
    end
    if counts then
        local n = counts.added + counts.changed
        if n > 0 then
            table.insert(parts, T(N_("1 highlight, note or bookmark", "%1 highlights, notes and bookmarks", n), n))
        end
        if counts.removed > 0 then
            table.insert(parts, T(N_("1 removed", "%1 removed", counts.removed), counts.removed))
        end
    end
    if books and books.failed > 0 then
        table.insert(parts, T(N_("1 book couldn't be fetched", "%1 books couldn't be fetched", books.failed), books.failed))
    end
    if #parts == 0 then
        return _("In step with Athene.")
    end
    return T(_("From Athene: %1."), table.concat(parts, ", "))
end

function Athene:syncNow()
    local account = self:account()
    if not account then
        self:explainSignIn()
        return
    end
    NetworkMgr:runWhenOnline(function()
        local client = Client.new(account)
        local info, code = client:info()
        if not info then
            UIManager:show(InfoMessage:new{ text = self:problemText(account, code) })
            return
        end
        local counts
        if self.ui.document then
            local failed
            counts, failed = self:syncBook(true)
            if failed then return end
        end
        local books = self:fetchBooks(true)
        if not books then return end
        self:checkForUpdate(false, info)
        UIManager:show(InfoMessage:new{ text = self:summary(counts, books), timeout = 4 })
    end)
end

function Athene:onAtheneSync()
    self:syncNow()
    return true
end

-- ---- keeping the plugin up to date ------------------------------------------------------------------

local function isNewer(a, b)
    local function parts(v)
        local out = {}
        for n in tostring(v or ""):gmatch("%d+") do table.insert(out, tonumber(n)) end
        return out
    end
    local pa, pb = parts(a), parts(b)
    for i = 1, math.max(#pa, #pb) do
        local x, y = pa[i] or 0, pb[i] or 0
        if x ~= y then return x > y end
    end
    return false
end

-- Notes whether the server carries a newer plugin; asks the server at most once a day.
function Athene:checkForUpdate(force, info)
    local now = os.time()
    if not force and not info and self.settings.update_checked and now - self.settings.update_checked < 86400 then
        return
    end
    if not info then
        local account = self:account()
        if not account then return end
        info = Client.new(account):info()
        if not info then return end
    end
    self.settings.update_checked = now
    self.settings.update_available = isNewer(info.plugin, self.version) and info.plugin or nil
    self:saveSettings()
end

function Athene:update()
    local account = self:account()
    if not account then return self:explainSignIn() end
    NetworkMgr:runWhenOnline(function()
        local client = Client.new(account)
        local code, body = client:pluginInfo()
        if code ~= 200 or type(body) ~= "table" or type(body.files) ~= "table" then
            UIManager:show(InfoMessage:new{ text = self:problemText(account, code) })
            return
        end
        local staging = self.path .. "/.update"
        ffiutil.purgeDir(staging)
        local sha256 = require("ffi/sha2").sha256
        for _, file in ipairs(body.files) do
            local rel = tostring(file.path or "")
            if rel == "" or rel:find("%.%.") or rel:sub(1, 1) == "/" then
                UIManager:show(InfoMessage:new{ text = _("The update looked wrong, so it wasn't installed.") })
                return
            end
            local target = staging .. "/" .. rel
            util.makePath((target:gsub("/[^/]*$", "")))
            local ok = client:download("/athene/v1/plugin/files/" .. rel, target)
            local data = ok and util.readFromFile(target, "rb")
            if not data or (file.sha256 and sha256(data) ~= file.sha256) then
                ffiutil.purgeDir(staging)
                UIManager:show(InfoMessage:new{ text = _("The update didn't download whole, so it wasn't installed. Try again.") })
                return
            end
        end
        for _, file in ipairs(body.files) do
            local target = self.path .. "/" .. file.path
            util.makePath((target:gsub("/[^/]*$", "")))
            os.rename(staging .. "/" .. file.path, target)
        end
        ffiutil.purgeDir(staging)
        self.settings.update_available = nil
        self:saveSettings()
        UIManager:askForRestart(T(_("The Athene plugin is now version %1. Restart KOReader to use it."), tostring(body.version)))
    end)
end

-- ---- the menu ----------------------------------------------------------------------------------------

local function when(ts)
    if not ts then return _("never") end
    local today = os.date("%Y-%m-%d")
    if os.date("%Y-%m-%d", ts) == today then
        return T(_("today, %1"), os.date("%H:%M", ts))
    end
    return os.date("%Y-%m-%d %H:%M", ts)
end

function Athene:addToMainMenu(menu_items)
    menu_items.athene = {
        text = _("Athene"),
        sorting_hint = "tools",
        sub_item_table_func = function() return self:menuItems() end,
    }
end

function Athene:menuItems()
    local account = self:account()
    return {
        {
            text = _("Sync now"),
            callback = function() self:syncNow() end,
        },
        {
            text = _("Sync automatically"),
            help_text = _("Highlights, notes and bookmarks go when a book opens, changes, or closes, and books from Athene arrive when the e-reader wakes on Wi-Fi. Wi-Fi is never turned on for this."),
            checked_func = function() return self.settings.auto_sync end,
            callback = function()
                self.settings.auto_sync = not self.settings.auto_sync
                self:saveSettings()
            end,
            separator = true,
        },
        {
            text_func = function() return T(_("Books from Athene go to: %1"), BD.dirpath(self:booksFolder())) end,
            keep_menu_open = true,
            callback = function(touchmenu_instance)
                local PathChooser = require("ui/widget/pathchooser")
                UIManager:show(PathChooser:new{
                    select_directory = true,
                    select_file = false,
                    path = self:booksFolder(),
                    onConfirm = function(path)
                        self.settings.folder = path
                        self:saveSettings()
                        if touchmenu_instance then touchmenu_instance:updateItems() end
                    end,
                })
            end,
        },
        {
            text_func = function() return T(_("Last synced: %1"), when(self.settings.last_sync)) end,
            keep_menu_open = true,
            callback = function() end,
        },
        {
            text_func = function()
                if not account then return _("Account: sign in with Progress sync") end
                return T(_("Account: %1 on %2"), account.username, self:serverName(account))
            end,
            keep_menu_open = true,
            callback = function()
                if not account then return self:explainSignIn() end
                UIManager:show(InfoMessage:new{
                    text = _("Athene uses Progress sync's account. To change it, open a book, then Tools ▸ Progress sync."),
                })
            end,
            separator = true,
        },
        {
            text_func = function()
                if self.settings.update_available then
                    return T(_("Athene plugin %1 · Update to %2"), self.version or "?", self.settings.update_available)
                end
                return T(_("Athene plugin %1"), self.version or "?")
            end,
            keep_menu_open = true,
            callback = function()
                if self.settings.update_available then
                    self:update()
                else
                    UIManager:show(InfoMessage:new{
                        text = T(_("Athene plugin %1. Your server offers updates when there are any."), self.version or "?"),
                    })
                end
            end,
        },
    }
end

return Athene
