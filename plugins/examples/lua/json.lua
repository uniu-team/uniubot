-- Minimal JSON codec for the Lua plugin: no luarocks, no C modules.
-- json.array{} always encodes as an array; empty tables encode as {}.

local json = {}

local array_mt = { __jsonarray = true }

function json.array(values)
  return setmetatable(values or {}, array_mt)
end

function json.is_array(value)
  if type(value) ~= "table" then
    return false
  end
  if getmetatable(value) == array_mt then
    return true
  end
  local count = 0
  for key in pairs(value) do
    if type(key) ~= "number" then
      return false
    end
    count = count + 1
  end
  for index = 1, count do
    if value[index] == nil then
      return false
    end
  end
  return count > 0
end

local escapes = {
  ['"'] = '\\"',
  ["\\"] = "\\\\",
  ["\b"] = "\\b",
  ["\f"] = "\\f",
  ["\n"] = "\\n",
  ["\r"] = "\\r",
  ["\t"] = "\\t",
}

local function encode_string(value)
  local out = value:gsub('[%z\1-\31"\\]', function(char)
    local escaped = escapes[char]
    if escaped then
      return escaped
    end
    return string.format("\\u%04x", string.byte(char))
  end)
  return '"' .. out .. '"'
end

local function encode_number(value)
  if value ~= value or value == math.huge or value == -math.huge then
    return "null"
  end
  if math.type and math.type(value) == "integer" then
    return string.format("%d", value)
  end
  if value == math.floor(value) and math.abs(value) < 1e15 then
    return string.format("%d", value)
  end
  return string.format("%.14g", value)
end

local function encode_value(value)
  local kind = type(value)
  if value == nil then
    return "null"
  elseif kind == "boolean" then
    return value and "true" or "false"
  elseif kind == "number" then
    return encode_number(value)
  elseif kind == "string" then
    return encode_string(value)
  elseif kind == "table" then
    local parts = {}
    if json.is_array(value) then
      local count = #value
      for index = 1, count do
        parts[#parts + 1] = encode_value(value[index])
      end
      return "[" .. table.concat(parts, ",") .. "]"
    end
    for key, item in pairs(value) do
      parts[#parts + 1] = encode_string(tostring(key)) .. ":" .. encode_value(item)
    end
    return "{" .. table.concat(parts, ",") .. "}"
  end
  return "null"
end

function json.encode(value)
  return encode_value(value)
end

-- ---- decoder ---------------------------------------------------------------

local function decode_error(message, position)
  error(string.format("json decode error at %d: %s", position or -1, message))
end

local function skip_whitespace(text, position)
  local _, last = text:find("^[ \t\r\n]*", position)
  return (last or position - 1) + 1
end

local decode_value

local function decode_string(text, position)
  if text:sub(position, position) ~= '"' then
    decode_error("expected string", position)
  end
  local out = {}
  local index = position + 1
  while true do
    local char = text:sub(index, index)
    if char == "" then
      decode_error("unterminated string", position)
    elseif char == '"' then
      return table.concat(out), index + 1
    elseif char == "\\" then
      local escape = text:sub(index + 1, index + 1)
      if escape == "u" then
        local hex = text:sub(index + 2, index + 5)
        local code = tonumber(hex, 16)
        if not code then
          decode_error("invalid unicode escape", index)
        end
        index = index + 6
        if code >= 0xD800 and code <= 0xDBFF then
          local low_hex = text:sub(index + 2, index + 5)
          local low = tonumber(low_hex, 16)
          if text:sub(index, index + 1) == "\\u" and low and low >= 0xDC00 and low <= 0xDFFF then
            code = 0x10000 + ((code - 0xD800) << 10) + (low - 0xDC00)
            index = index + 6
          end
        end
        out[#out + 1] = utf8.char(code)
      else
        local map = { b = "\b", f = "\f", n = "\n", r = "\r", t = "\t", ['"'] = '"', ["\\"] = "\\", ["/"] = "/" }
        local mapped = map[escape]
        if not mapped then
          decode_error("invalid escape", index)
        end
        out[#out + 1] = mapped
        index = index + 2
      end
    else
      out[#out + 1] = char
      index = index + 1
    end
  end
end

local function decode_number(text, position)
  local number_text = text:match("^-?%d+%.?%d*[eE]?[-+]?%d*", position)
  if not number_text or number_text == "" then
    decode_error("invalid number", position)
  end
  local value = tonumber(number_text)
  if not value then
    decode_error("invalid number", position)
  end
  return value, position + #number_text
end

local function decode_array(text, position)
  local result = json.array({})
  local index = skip_whitespace(text, position + 1)
  if text:sub(index, index) == "]" then
    return result, index + 1
  end
  while true do
    local value
    value, index = decode_value(text, index)
    result[#result + 1] = value
    index = skip_whitespace(text, index)
    local char = text:sub(index, index)
    if char == "]" then
      return result, index + 1
    elseif char == "," then
      index = skip_whitespace(text, index + 1)
    else
      decode_error("expected ',' or ']'", index)
    end
  end
end

local function decode_object(text, position)
  local result = {}
  local index = skip_whitespace(text, position + 1)
  if text:sub(index, index) == "}" then
    return result, index + 1
  end
  while true do
    local key
    key, index = decode_string(text, index)
    index = skip_whitespace(text, index)
    if text:sub(index, index) ~= ":" then
      decode_error("expected ':'", index)
    end
    index = skip_whitespace(text, index + 1)
    local value
    value, index = decode_value(text, index)
    result[key] = value
    index = skip_whitespace(text, index)
    local char = text:sub(index, index)
    if char == "}" then
      return result, index + 1
    elseif char == "," then
      index = skip_whitespace(text, index + 1)
    else
      decode_error("expected ',' or '}'", index)
    end
  end
end

decode_value = function(text, position)
  position = skip_whitespace(text, position)
  local char = text:sub(position, position)
  if char == "{" then
    return decode_object(text, position)
  elseif char == "[" then
    return decode_array(text, position)
  elseif char == '"' then
    return decode_string(text, position)
  elseif char == "t" and text:sub(position, position + 3) == "true" then
    return true, position + 4
  elseif char == "f" and text:sub(position, position + 4) == "false" then
    return false, position + 5
  elseif char == "n" and text:sub(position, position + 3) == "null" then
    return nil, position + 4
  elseif char == "-" or char:match("%d") then
    return decode_number(text, position)
  end
  decode_error("unexpected character '" .. char .. "'", position)
end

function json.decode(text)
  local value, position = decode_value(text, 1)
  position = skip_whitespace(text, position)
  if position <= #text then
    decode_error("trailing characters", position)
  end
  return value
end

return json
