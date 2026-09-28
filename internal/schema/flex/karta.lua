-- Karta osm2pgsql flex configuration (schema major version 1).
--
-- Writes raw layer tables into the `karta` schema of a release database.
-- Post-import SQL (internal/schema/sql) repairs and clips geometry, derives
-- label points, zoom thresholds, search names and the tile function.
-- Requires osm2pgsql >= 1.11. All geometry is Web Mercator (EPSG:3857).

local schema = 'karta'
local srid = 3857

local tables = {}

tables.roads = osm2pgsql.define_table({
    name = 'roads', schema = schema,
    ids = { type = 'way', id_column = 'osm_id' },
    columns = {
        { column = 'class', type = 'text', not_null = true },
        { column = 'subclass', type = 'text', not_null = true },
        { column = 'name', type = 'text' },
        { column = 'name_fa', type = 'text' },
        { column = 'name_en', type = 'text' },
        { column = 'ref', type = 'text' },
        { column = 'oneway', type = 'bool', not_null = true },
        { column = 'bridge', type = 'bool', not_null = true },
        { column = 'tunnel', type = 'bool', not_null = true },
        { column = 'layer', type = 'int2', not_null = true },
        { column = 'geom', type = 'geometry', projection = srid, not_null = true },
    },
})

tables.waterways = osm2pgsql.define_table({
    name = 'waterways', schema = schema,
    ids = { type = 'way', id_column = 'osm_id' },
    columns = {
        { column = 'kind', type = 'text', not_null = true },
        { column = 'name', type = 'text' },
        { column = 'name_fa', type = 'text' },
        { column = 'name_en', type = 'text' },
        { column = 'tunnel', type = 'bool', not_null = true },
        { column = 'geom', type = 'geometry', projection = srid, not_null = true },
    },
})

tables.water = osm2pgsql.define_table({
    name = 'water', schema = schema,
    ids = { type = 'any', id_column = 'osm_id', type_column = 'osm_type' },
    columns = {
        { column = 'kind', type = 'text', not_null = true },
        { column = 'name', type = 'text' },
        { column = 'name_fa', type = 'text' },
        { column = 'name_en', type = 'text' },
        { column = 'geom', type = 'geometry', projection = srid, not_null = true },
    },
})

tables.landcover = osm2pgsql.define_table({
    name = 'landcover', schema = schema,
    ids = { type = 'any', id_column = 'osm_id', type_column = 'osm_type' },
    columns = {
        { column = 'class', type = 'text', not_null = true },
        { column = 'kind', type = 'text', not_null = true },
        { column = 'geom', type = 'geometry', projection = srid, not_null = true },
    },
})

tables.buildings = osm2pgsql.define_table({
    name = 'buildings', schema = schema,
    ids = { type = 'any', id_column = 'osm_id', type_column = 'osm_type' },
    columns = {
        { column = 'kind', type = 'text', not_null = true },
        { column = 'geom', type = 'geometry', projection = srid, not_null = true },
    },
})

-- Named places and points of interest: the source for search and labels.
tables.features = osm2pgsql.define_table({
    name = 'features', schema = schema,
    ids = { type = 'any', id_column = 'osm_id', type_column = 'osm_type' },
    columns = {
        { column = 'category', type = 'text', not_null = true },
        { column = 'subcategory', type = 'text', not_null = true },
        { column = 'names', type = 'jsonb', not_null = true },
        { column = 'geom', type = 'geometry', projection = srid, not_null = true },
    },
})

-- Objects that matched a layer but produced no usable geometry (for example
-- a relation whose members are outside the extract). Reported by the importer.
tables.skipped = osm2pgsql.define_table({
    name = 'import_skipped', schema = schema,
    ids = { type = 'any', id_column = 'osm_id', type_column = 'osm_type' },
    columns = {
        { column = 'layer', type = 'text', not_null = true },
        { column = 'reason', type = 'text', not_null = true },
    },
})

local road_classes = {
    motorway = 'motorway', motorway_link = 'motorway',
    trunk = 'trunk', trunk_link = 'trunk',
    primary = 'primary', primary_link = 'primary',
    secondary = 'secondary', secondary_link = 'secondary',
    tertiary = 'tertiary', tertiary_link = 'tertiary',
    residential = 'minor', unclassified = 'minor', living_street = 'minor', road = 'minor',
    service = 'service', track = 'track',
    pedestrian = 'path', footway = 'path', path = 'path', cycleway = 'path',
    steps = 'path', bridleway = 'path',
}

local rail_classes = {
    rail = true, light_rail = true, subway = true, tram = true,
    narrow_gauge = true, monorail = true,
}

local waterway_kinds = { river = true, stream = true, canal = true, drain = true, ditch = true }

local water_kinds = {
    natural = { water = true },
    waterway = { riverbank = 'river', dock = 'dock' },
    landuse = { reservoir = 'reservoir', basin = 'basin' },
}

-- Key order decides the class when an object carries several tags.
local landcover_keys = { 'leisure', 'landuse', 'natural', 'amenity' }
local landcover_classes = {
    leisure = {
        park = 'park', garden = 'park', playground = 'park', nature_reserve = 'park',
        dog_park = 'park', recreation_ground = 'park', golf_course = 'park',
        pitch = 'sport', sports_centre = 'sport', stadium = 'sport', track = 'sport',
    },
    landuse = {
        grass = 'grass', meadow = 'grass', village_green = 'grass',
        recreation_ground = 'park', forest = 'wood',
        orchard = 'farmland', vineyard = 'farmland', farmland = 'farmland', farmyard = 'farmland',
        allotments = 'farmland', plant_nursery = 'farmland', greenhouse_horticulture = 'farmland',
        cemetery = 'cemetery', residential = 'residential', commercial = 'commercial',
        retail = 'commercial', industrial = 'industrial', railway = 'industrial',
        military = 'industrial', construction = 'construction', brownfield = 'construction',
        greenfield = 'construction', education = 'education',
    },
    natural = {
        wood = 'wood', scrub = 'wood', grassland = 'grass', heath = 'grass',
        sand = 'bare', bare_rock = 'bare', scree = 'bare', beach = 'bare', wetland = 'wetland',
    },
    amenity = {
        school = 'education', university = 'education', college = 'education',
        hospital = 'hospital', grave_yard = 'cemetery', parking = 'parking',
    },
}

-- Keys that make a named object a searchable place or POI, in priority order.
-- A value of `true` accepts any value; a table lists accepted values.
local feature_keys = {
    { 'place', true }, { 'amenity', true }, { 'shop', true }, { 'tourism', true },
    { 'leisure', true }, { 'historic', true }, { 'natural', true }, { 'waterway', true },
    { 'water', true }, { 'aeroway', true }, { 'office', true }, { 'healthcare', true },
    { 'craft', true }, { 'emergency', true }, { 'man_made', true }, { 'sport', true },
    { 'club', true },
    { 'railway', { station = true, halt = true, tram_stop = true, subway_entrance = true } },
    { 'public_transport', true },
    { 'highway', { bus_stop = true } },
    { 'landuse', true },
    { 'building', true },
}

-- Ways with these keys are linear even when closed (a closed road is a loop).
local linear_keys = { 'highway', 'waterway', 'railway', 'barrier', 'aerialway', 'power' }

-- Name tags that are indexed for search. An optional language suffix of two
-- or three lowercase letters is accepted (name:fa, alt_name:en, ...).
local name_bases = {
    name = true, alt_name = true, official_name = true, short_name = true,
    old_name = true, int_name = true, loc_name = true, reg_name = true, nat_name = true,
}
local max_name_bytes = 500

local function clean(value)
    if value == nil then return nil end
    value = value:gsub('^%s+', ''):gsub('%s+$', '')
    if value == '' then return nil end
    return value
end

local function yes(value)
    return value == 'yes' or value == 'true' or value == '1'
end

local function layer_value(value)
    local number = tonumber(value)
    if number == nil or number ~= math.floor(number) or number < -10 or number > 10 then
        return 0
    end
    return number
end

local function is_name_key(key)
    local base, lang = key:match('^([%l_]+):(%l%l%l?)$')
    if base == nil then
        return name_bases[key] == true
    end
    return name_bases[base] == true and lang ~= nil
end

local function collect_names(tags)
    local names = {}
    local found = false
    for key, value in pairs(tags) do
        if is_name_key(key) then
            local v = clean(value)
            if v ~= nil and #v <= max_name_bytes then
                names[key] = v
                found = true
            end
        end
    end
    if found then return names end
    return nil
end

local function is_linear(tags)
    if tags.area == 'yes' or tags.waterway == 'riverbank' or tags.waterway == 'dock' then
        return false
    end
    for _, key in ipairs(linear_keys) do
        if tags[key] ~= nil then return true end
    end
    return false
end

local function skip(object, layer, reason)
    tables.skipped:insert({ layer = layer, reason = reason })
end

-- Returns an areal geometry for a closed way or multipolygon relation, or nil.
local function area_geometry(object, osm_type)
    if osm_type == 'way' then
        if not object.is_closed or object.tags.area == 'no' then return nil end
        return object:as_polygon()
    end
    return object:as_multipolygon()
end

local function insert_area(tbl, layer, object, osm_type, row)
    local geom = area_geometry(object, osm_type)
    if geom == nil then return end
    if geom:is_null() then
        skip(object, layer, 'invalid_or_incomplete_area')
        return
    end
    row.geom = geom
    tbl:insert(row)
end

local function process_areas(object, osm_type)
    local tags = object.tags

    local water_kind = nil
    if tags.natural == 'water' then
        water_kind = clean(tags.water) or 'water'
    elseif water_kinds.waterway[tags.waterway] then
        water_kind = water_kinds.waterway[tags.waterway]
    elseif water_kinds.landuse[tags.landuse] then
        water_kind = water_kinds.landuse[tags.landuse]
    end
    if water_kind ~= nil then
        insert_area(tables.water, 'water', object, osm_type, {
            kind = water_kind, name = clean(tags.name),
            name_fa = clean(tags['name:fa']), name_en = clean(tags['name:en']),
        })
    end

    for _, key in ipairs(landcover_keys) do
        local class = landcover_classes[key][tags[key]]
        if class ~= nil then
            insert_area(tables.landcover, 'landcover', object, osm_type, {
                class = class, kind = key .. '=' .. tags[key],
            })
            break
        end
    end

    local building = tags.building
    if building ~= nil and building ~= 'no' then
        insert_area(tables.buildings, 'buildings', object, osm_type, { kind = building })
    end
end

local function feature_category(tags)
    for _, entry in ipairs(feature_keys) do
        local key, accepted = entry[1], entry[2]
        local value = tags[key]
        if value ~= nil and value ~= 'no' then
            if accepted == true or accepted[value] then
                return key, value
            end
        end
    end
    return nil, nil
end

local function process_feature(object, osm_type)
    local names = collect_names(object.tags)
    if names == nil then return end
    local category, subcategory = feature_category(object.tags)
    if category == nil then return end

    local geom
    if osm_type == 'node' then
        geom = object:as_point()
    elseif osm_type == 'way' then
        if object.is_closed and not is_linear(object.tags) and object.tags.area ~= 'no' then
            geom = object:as_polygon()
        else
            geom = object:as_linestring()
        end
    else
        local relation_type = object.tags.type
        if relation_type ~= 'multipolygon' and relation_type ~= 'boundary' then
            skip(object, 'features', 'unsupported_relation_type')
            return
        end
        geom = object:as_multipolygon()
    end
    if geom:is_null() then
        skip(object, 'features', 'invalid_or_incomplete_geometry')
        return
    end
    tables.features:insert({
        category = category, subcategory = subcategory, names = names, geom = geom,
    })
end

function osm2pgsql.process_node(object)
    process_feature(object, 'node')
end

function osm2pgsql.process_way(object)
    local tags = object.tags

    local highway = tags.highway
    local road_class = road_classes[highway]
    if road_class ~= nil and tags.area ~= 'yes' then
        local geom = object:as_linestring()
        if geom:is_null() then
            skip(object, 'roads', 'invalid_linestring')
        else
            tables.roads:insert({
                class = road_class, subclass = highway,
                name = clean(tags.name), name_fa = clean(tags['name:fa']),
                name_en = clean(tags['name:en']), ref = clean(tags.ref),
                oneway = yes(tags.oneway), bridge = tags.bridge ~= nil and tags.bridge ~= 'no',
                tunnel = tags.tunnel ~= nil and tags.tunnel ~= 'no',
                layer = layer_value(tags.layer), geom = geom,
            })
        end
    elseif rail_classes[tags.railway] then
        local geom = object:as_linestring()
        if geom:is_null() then
            skip(object, 'roads', 'invalid_linestring')
        else
            tables.roads:insert({
                class = 'rail', subclass = tags.railway,
                name = clean(tags.name), name_fa = clean(tags['name:fa']),
                name_en = clean(tags['name:en']), ref = clean(tags.ref),
                oneway = false, bridge = tags.bridge ~= nil and tags.bridge ~= 'no',
                tunnel = tags.tunnel ~= nil and tags.tunnel ~= 'no',
                layer = layer_value(tags.layer), geom = geom,
            })
        end
    end

    if waterway_kinds[tags.waterway] then
        local geom = object:as_linestring()
        if geom:is_null() then
            skip(object, 'waterways', 'invalid_linestring')
        else
            tables.waterways:insert({
                kind = tags.waterway, name = clean(tags.name),
                name_fa = clean(tags['name:fa']), name_en = clean(tags['name:en']),
                tunnel = tags.tunnel ~= nil and tags.tunnel ~= 'no', geom = geom,
            })
        end
    end

    -- Area layers require their own tags, so a closed road loop never becomes one.
    process_areas(object, 'way')
    process_feature(object, 'way')
end

function osm2pgsql.process_relation(object)
    if object.tags.type == 'multipolygon' then
        process_areas(object, 'relation')
    end
    process_feature(object, 'relation')
end
