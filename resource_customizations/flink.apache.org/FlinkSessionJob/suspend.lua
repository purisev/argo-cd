if obj.spec ~= nil and obj.spec.job ~= nil then
  obj.spec.job.state = "suspended"
end
return obj
