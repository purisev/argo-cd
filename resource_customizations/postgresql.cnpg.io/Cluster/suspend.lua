if obj.metadata.annotations == nil then
  obj.metadata.annotations = {}
end
obj.metadata.annotations["cnpg.io/hibernation"] = "on"
return obj
