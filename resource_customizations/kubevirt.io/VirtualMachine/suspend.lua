if obj.spec == nil then
  obj.spec = {}
end
if obj.spec.running ~= nil then
  obj.spec.running = false
else
  obj.spec.runStrategy = "Halted"
end
return obj
