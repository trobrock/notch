notch.register_tool({
  name = "hello",
  description = "Return a greeting for a name",
  input_schema = {
    type = "object",
    properties = {
      name = { type = "string", description = "Name to greet" },
    },
    required = { "name" },
  },
  execute = function(args, update)
    update("building greeting")
    return { content = "Hello, " .. args.name .. " from Lua." }
  end,
})

notch.register_command({
  name = "hello",
  description = "Greet someone without calling a model",
  execute = function(args)
    if args == "" then args = "world" end
    return "Hello, " .. args .. "."
  end,
})
