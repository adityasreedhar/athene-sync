--[[--
KOReader's annotations, as the records the Athene server keeps, and back.

A record is one highlight, note or bookmark: an id, when it last changed (`updated`, in
milliseconds), and what KOReader knows of it. Its place is KOReader's own: XPointers in a book
crengine reads, a page and points in a PDF. KOReader's annotations carry the record's id in
`athene_id`; one made on this e-reader gets an id the first time it goes.
]]

local md5 = require("ffi/sha2").md5

local Records = {}

--- KOReader's "2026-10-05 14:03:07" (local time), in milliseconds since 1970; 0 when unreadable.
function Records.stamp(datetime)
    if type(datetime) ~= "string" then return 0 end
    local y, mo, d, h, mi, s = datetime:match("^(%d+)-(%d+)-(%d+)[ T](%d+):(%d+):(%d+)")
    if not y then return 0 end
    local t = os.time{ year = tonumber(y), month = tonumber(mo), day = tonumber(d),
                       hour = tonumber(h), min = tonumber(mi), sec = tonumber(s) }
    return t and t * 1000 or 0
end

function Records.datetime(ms)
    return os.date("%Y-%m-%d %H:%M:%S", math.floor((tonumber(ms) or 0) / 1000))
end

function Records.now()
    return os.time() * 1000
end

--- When the annotation last changed here.
function Records.itemStamp(item)
    return Records.stamp(item.datetime_updated or item.datetime)
end

local function positionKey(pos)
    if type(pos) == "table" then
        return string.format("%s:%s:%s", tostring(pos.page), tostring(pos.x), tostring(pos.y))
    end
    return tostring(pos or "")
end

--- Where an annotation is, as one string: two at the same place are the same annotation.
function Records.placeKey(item)
    if item.drawer or item.kind == "highlight" then
        return "h|" .. positionKey(item.pos0) .. "|" .. positionKey(item.pos1)
    end
    return "b|" .. positionKey(item.page)
end

--- An id for an annotation made here: from where it is and when it was made, so the same
--- annotation gets the same id even if KOReader never saved the one it was given.
function Records.newID(item)
    return "ko-" .. md5(Records.placeKey(item) .. "|" .. tostring(item.datetime or "")):sub(1, 24)
end

local function copyPosition(pos)
    if type(pos) ~= "table" then return pos end
    local out = {}
    for _, k in ipairs({ "page", "x", "y", "zoom", "rotation" }) do
        if type(pos[k]) == "number" then out[k] = pos[k] end
    end
    return out
end

--- A KOReader annotation as a record.
function Records.fromItem(item, device, device_id)
    local rec = {
        id = item.athene_id,
        updated = Records.itemStamp(item),
        created = Records.stamp(item.datetime),
        kind = item.drawer and "highlight" or "bookmark",
        page = item.page,
        chapter = item.chapter,
        text = item.text,
        device = device,
        device_id = device_id,
    }
    if item.drawer then
        rec.pos0 = copyPosition(item.pos0)
        rec.pos1 = copyPosition(item.pos1)
        rec.note = item.note
        rec.color = item.color
        rec.drawer = item.drawer
    end
    if rec.updated == 0 then rec.updated = Records.now() end
    if rec.created == 0 then rec.created = rec.updated end
    return rec
end

local DRAWERS = { lighten = true, underscore = true, strikeout = true, invert = true }

--- A record as a KOReader annotation, or nil when it isn't one this book can show.
--- `paging`: the book is a PDF (or another paged format), where places are pages and points.
function Records.toItem(rec, paging)
    if type(rec) ~= "table" or rec.deleted or type(rec.id) ~= "string" then return nil end
    local created = tonumber(rec.created) or tonumber(rec.updated) or Records.now()
    local updated = tonumber(rec.updated) or created
    local item = {
        athene_id = rec.id,
        datetime = Records.datetime(created),
        chapter = type(rec.chapter) == "string" and rec.chapter or nil,
        text = type(rec.text) == "string" and rec.text or nil,
    }
    if math.floor(updated / 1000) > math.floor(created / 1000) then
        item.datetime_updated = Records.datetime(updated)
    end
    if rec.kind == "highlight" then
        if paging then
            if type(rec.pos0) ~= "table" or type(rec.pos1) ~= "table" then return nil end
            item.pos0 = copyPosition(rec.pos0)
            item.pos1 = copyPosition(rec.pos1)
            item.page = tonumber(rec.page) or item.pos0.page
            if type(item.page) ~= "number" then return nil end
            item.pos0.page = item.pos0.page or item.page
            item.pos1.page = item.pos1.page or item.page
        else
            if type(rec.pos0) ~= "string" or type(rec.pos1) ~= "string" then return nil end
            item.pos0, item.pos1, item.page = rec.pos0, rec.pos1, rec.pos0
        end
        item.drawer = DRAWERS[rec.drawer] and rec.drawer or "lighten"
        item.color = type(rec.color) == "string" and rec.color or "yellow"
        item.note = type(rec.note) == "string" and rec.note ~= "" and rec.note or nil
    elseif rec.kind == "bookmark" then
        if paging then
            item.page = tonumber(rec.page)
            if not item.page then return nil end
        else
            if type(rec.page) ~= "string" then return nil end
            item.page = rec.page
        end
    else
        return nil
    end
    return item
end

--- Whether an annotation already says what a record says.
function Records.sameContent(item, rec)
    if (item.drawer and "highlight" or "bookmark") ~= rec.kind then return false end
    if Records.placeKey(item) ~= Records.placeKey(rec) then return false end
    if item.drawer then
        return (item.note or "") == (rec.note or "") and item.color == rec.color
            and item.drawer == rec.drawer and (item.text or "") == (rec.text or "")
    end
    return (item.text or "") == (rec.text or "")
end

return Records
